package sweep

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func Run(ctx context.Context, cfg Config) (batch Batch, err error) {
	defer func() {
		raw, _ := json.Marshal(redactValue(batch, cfg))
		json.Unmarshal(raw, &batch)
	}()
	batch = Batch{SchemaVersion: 1, ID: newID(), Status: "running", StartedAt: time.Now().UTC(), Selections: cfg.Connections, Repositories: []Outcome{}, DiscoveryComplete: true}
	version, err := scannerIdentity(cfg.Deplens)
	if err != nil {
		return batch, err
	}
	lock, canonical, err := lockResults(cfg.ResultsDir)
	if err != nil {
		return batch, err
	}
	defer lock.Close()
	cfg.ResultsDir = canonical
	work, owner, err := prepareWorkspace(cfg, batch.ID)
	if err != nil {
		return batch, err
	}
	defer removeWorkspace(work)
	ctx = context.WithValue(ctx, workOwnerKey{}, owner)
	stage, err := prepareStaging(cfg, owner)
	if err != nil {
		return batch, err
	}
	defer os.RemoveAll(stage)
	askpass, err := writeAskpass(work)
	if err != nil {
		return batch, errors.New("cannot prepare Git authentication")
	}
	if err = saveBatch(cfg, &batch); err != nil {
		return batch, err
	}
	defer func() {
		if batch.Status == "running" {
			batch.Status = "incomplete"
			batch.CompletedAt = time.Now().UTC()
			if saveErr := saveBatch(cfg, &batch); err == nil {
				err = saveErr
			}
		}
	}()
	for _, c := range cfg.Connections {
		var repos []Repository
		var failures []Outcome
		if c.Provider == "github" {
			repos, failures = discoverGitHub(ctx, c)
		} else {
			repos, failures = discoverGitLab(ctx, c)
		}
		if len(failures) > 0 {
			batch.DiscoveryComplete = false
		}
		for _, failure := range failures {
			batch.Repositories = append(batch.Repositories, failure)
			if err = saveBatch(cfg, &batch); err != nil {
				return batch, err
			}
		}
		if err = processRepositories(ctx, cfg, c, repos, version, work, askpass, &batch); err != nil {
			return batch, err
		}
	}
	batch.CompletedAt = time.Now().UTC()
	batch.Status = "complete"
	if !batch.DiscoveryComplete || batch.Counts["failed"] > 0 {
		batch.Status = "incomplete"
	}
	if err = saveBatch(cfg, &batch); err != nil {
		return batch, err
	}
	if !batch.DiscoveryComplete || batch.Counts["failed"] > 0 {
		return batch, errors.New("batch is incomplete; see saved repository outcomes")
	}
	return batch, nil
}

// The coordinator owns batch mutation and starts new work only after a completed
// attempt has persisted its result and removed its checkout.
func processRepositories(ctx context.Context, cfg Config, c Connection, repos []Repository, version, work, askpass string, batch *Batch) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type completion struct {
		outcome Outcome
		err     error
	}
	completed := make(chan completion, cfg.Workers)
	next, active := 0, 0
	var failure error
	for next < len(repos) || active > 0 {
		if ctx.Err() != nil && next < len(repos) {
			for _, repo := range repos[next:] {
				batch.Repositories = append(batch.Repositories, Outcome{Repository: repo, CheckedAt: time.Now().UTC(), Outcome: "unprocessed", Error: "batch canceled before attempt started"})
			}
			next = len(repos)
			if failure == nil {
				failure = errors.New("batch canceled")
			}
		}
		for next < len(repos) && active < cfg.Workers {
			repo := repos[next]
			next++
			active++
			go func() {
				outcome, err := processRepository(ctx, cfg, c, repo, version, work, askpass)
				completed <- completion{outcome, err}
			}()
		}
		if active == 0 {
			break
		}
		result := <-completed
		active--
		batch.Repositories = append(batch.Repositories, result.outcome)
		if result.err != nil && failure == nil {
			failure = result.err
			cancel()
		}
		if err := saveBatch(cfg, batch); err != nil && failure == nil {
			failure = err
			cancel()
		}
	}
	if err := saveBatch(cfg, batch); err != nil && failure == nil {
		failure = err
	}
	return failure
}
func processRepository(ctx context.Context, cfg Config, c Connection, repo Repository, version, work, askpass string) (Outcome, error) {
	started := time.Now().UTC()
	result := Outcome{Repository: repo, CheckedAt: started, Outcome: "empty"}
	if repo.Empty {
		return result, nil
	}
	fingerprint, fingerprintErr := inputFingerprint(cfg.Deplens, version)
	if fingerprintErr != nil {
		return Outcome{Repository: repo, Outcome: "failed", CheckedAt: started, Error: fingerprintErr.Error()}, nil
	}
	if reused, ok := findReusable(cfg, repo, fingerprint); ok {
		return reused, nil
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	ref := filepath.Join(repositoryDir(repo), "attempts", newID())
	checkout := filepath.Join(work, "checkout-"+newID())
	meta := Metadata{Repository: repo, ScannerVersion: version, InputFingerprint: fingerprint, Configuration: cfg.Deplens, Scope: Scope{Shallow: true}, StartedAt: started, Outcome: "failed"}
	err := acquire(ctx, c, repo, checkout, askpass)
	var output any
	if err == nil {
		scanned := time.Now().UTC()
		meta.ScannedAt = &scanned
		result.ScannedAt = &scanned
		output, err = analyze(ctx, cfg, checkout)
		if err == nil {
			after, fingerprintErr := inputFingerprint(cfg.Deplens, version)
			if fingerprintErr != nil || after != fingerprint {
				err = errors.New("scan configuration changed during analysis")
			}
		}
	}
	cleanupErr := os.RemoveAll(checkout)
	if cleanupErr != nil {
		err = errors.New("cannot remove temporary checkout")
	}
	result.Outcome = "complete"
	meta.Outcome = "complete"
	if err != nil {
		result.Outcome = "failed"
		meta.Outcome = "failed"
		result.Error = err.Error()
		meta.Error = err.Error()
	}
	meta.CompletedAt = time.Now().UTC()
	storageFailure := func(message string) (Outcome, error) {
		result.Outcome = "failed"
		result.Error = message
		return result, errors.New(message)
	}
	if saveErr := saveAttempt(ctx, cfg, ref, output, meta); saveErr != nil {
		return storageFailure(saveErr.Error())
	}
	result.Result = filepath.ToSlash(ref)
	if result.Outcome == "complete" {
		if saveErr := saveJSON(filepath.Join(cfg.ResultsDir, repositoryDir(repo), "latest.json"), redactValue(result, cfg)); saveErr != nil {
			return storageFailure("cannot publish complete result")
		}
	}
	if cleanupErr != nil {
		return result, errors.New("cannot remove temporary checkout")
	}
	return result, nil
}
func redactValue(value any, cfg Config) any {
	// Decode typed metadata too so every persisted string uses the same redaction.
	raw, _ := json.Marshal(value)
	var decoded any
	json.Unmarshal(raw, &decoded)
	var redact func(any) any
	redact = func(v any) any {
		switch item := v.(type) {
		case string:
			for _, c := range cfg.Connections {
				if c.token != "" {
					item = strings.ReplaceAll(item, c.token, "[REDACTED]")
				}
			}
			return item
		case []any:
			for i, x := range item {
				item[i] = redact(x)
			}
		case map[string]any:
			result := make(map[string]any, len(item))
			for k, x := range item {
				result[redact(k).(string)] = redact(x)
			}
			return result
		}
		return v
	}
	return redact(decoded)
}
