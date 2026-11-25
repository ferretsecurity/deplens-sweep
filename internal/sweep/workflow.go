package sweep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
		cfg.progress("%s: discovering %s repositories", c.Name, c.Provider)
		// Discovery feeds the workers while it runs, so the first clones do not wait
		// for the whole walk. The coordinator drains repos until discovery closes it.
		repos := make(chan Repository, 256)
		var failures []Outcome
		// Summaries saved while discovery runs must not claim a complete discovery.
		discovered := batch.DiscoveryComplete
		batch.DiscoveryComplete = false
		go func() {
			defer close(repos)
			if c.Provider == "github" {
				var found []Repository
				found, failures = discoverGitHub(ctx, c, cfg)
				cfg.progress("%s: discovery finished (selected: %d, errors: %d)", c.Name, len(found), len(failures))
				for _, repo := range found {
					repos <- repo
				}
				return
			}
			failures = discoverGitLab(ctx, c, func(group string) {
				fmt.Fprintf(os.Stderr, "GitLab connection %q group %q: switching to explicit subgroup walking after repeated HTTP 500\n", redactValue(c.Name, cfg), redactValue(group, cfg))
			}, func(repo Repository) { repos <- repo })
		}()
		processErr := processRepositories(ctx, cfg, c, repos, version, work, askpass, &batch)
		// processRepositories returns only after repos is closed, so failures is final.
		batch.DiscoveryComplete = discovered && len(failures) == 0
		if len(failures) > 0 {
			for _, result := range failures {
				batch.Repositories = append(batch.Repositories, result)
				cfg.logOutcome(len(batch.Repositories), result)
			}
			if err = saveBatch(cfg, &batch); err != nil {
				return batch, err
			}
		}
		if err = processErr; err != nil {
			return batch, err
		}
	}
	batch.CompletedAt = time.Now().UTC()
	batch.Status = "complete"
	if !batch.DiscoveryComplete || batch.Counts["failed"] > 0 || batch.Counts["unprocessed"] > 0 || batch.ReviewCounts["failed"] > 0 {
		batch.Status = "incomplete"
	}
	if err = saveBatch(cfg, &batch); err != nil {
		return batch, err
	}
	cfg.progress("Batch %s: %s. Outcomes: %d. Summary: %s", batch.ID, batch.Status, len(batch.Repositories), filepath.Join(cfg.ResultsDir, "batches", batch.ID+".json"))
	if batch.Status == "incomplete" {
		return batch, errors.New("batch is incomplete; see saved repository outcomes")
	}
	return batch, nil
}

const batchSaveInterval = 10 * time.Second

// The coordinator owns batch mutation and starts new work only after a completed
// attempt has persisted its result and removed its checkout. It always receives
// from repos until the channel is closed, so discovery never blocks on workers.
func processRepositories(ctx context.Context, cfg Config, c Connection, repos <-chan Repository, version, work, askpass string, batch *Batch) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type completion struct {
		outcome Outcome
		err     error
	}
	completed := make(chan completion, cfg.Workers)
	var queue []Repository
	open, active := true, 0
	var failure error
	var pendingFingerprint string
	var pendingReuse *Outcome
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	// Rewriting the whole summary costs time proportional to its size, so saving it
	// after every outcome makes a large sweep quadratic. Attempts and latest
	// references are saved immediately; the summary is saved when an outcome
	// arrives at least one interval after the last save, and always before returning.
	var saved time.Time
	persist := func() {
		if time.Since(saved) < batchSaveInterval {
			return
		}
		saved = time.Now()
		if err := saveBatch(cfg, batch); err != nil && failure == nil {
			failure = err
			cancel()
		}
	}
	record := func(result Outcome) {
		batch.Repositories = append(batch.Repositories, result)
		cfg.logOutcome(len(batch.Repositories), result)
		persist()
	}
	for open || len(queue) > 0 || active > 0 {
		if ctx.Err() != nil && len(queue) > 0 {
			for i, repo := range queue {
				if i == 0 && pendingReuse != nil {
					pendingReuse.Review = &ReviewOutcome{Status: "failed", Error: "batch canceled before review started"}
					batch.Repositories = append(batch.Repositories, *pendingReuse)
					continue
				}
				batch.Repositories = append(batch.Repositories, Outcome{Repository: repo, CheckedAt: time.Now().UTC(), Outcome: "unprocessed", Error: "batch canceled before attempt started"})
			}
			queue = queue[:0]
			pendingFingerprint = ""
			pendingReuse = nil
			if failure == nil {
				failure = errors.New("batch canceled")
			}
		}
		for len(queue) > 0 && active < cfg.Workers && ctx.Err() == nil {
			repo := queue[0]
			if repo.Empty {
				queue = queue[1:]
				record(Outcome{Repository: repo, CheckedAt: time.Now().UTC(), Outcome: "empty"})
				continue
			}
			if pendingFingerprint == "" {
				fingerprint, reused, ok, reuseErr := prepareRepository(cfg, repo, version)
				if reuseErr != nil {
					queue = queue[1:]
					record(Outcome{Repository: repo, CheckedAt: time.Now().UTC(), Outcome: "failed", Error: reuseErr.Error()})
					continue
				}
				if ok {
					if !cfg.Review {
						queue = queue[1:]
						record(reused)
						continue
					}
					if review, valid := reusableReview(cfg, reused); valid {
						queue = queue[1:]
						reused.Review = &review
						record(reused)
						continue
					}
					pendingReuse = &reused
				}
				pendingFingerprint = fingerprint
			}
			available, spaceErr := cloneSpaceAvailable(work, *cfg.MinFreeBytes)
			if spaceErr == nil && !available && active > 0 {
				break
			}
			if spaceErr != nil || !available {
				message := "insufficient free space for temporary checkout; repository was not started"
				if spaceErr != nil {
					message = spaceErr.Error()
				}
				queue = queue[1:]
				pendingFingerprint = ""
				if pendingReuse != nil {
					pendingReuse.Review = &ReviewOutcome{Status: "failed", Error: message}
					record(*pendingReuse)
					pendingReuse = nil
				} else {
					record(Outcome{Repository: repo, CheckedAt: time.Now().UTC(), Outcome: "unprocessed", Error: message})
				}
				continue
			}
			queue = queue[1:]
			active++
			fingerprint := pendingFingerprint
			reused := pendingReuse
			pendingFingerprint = ""
			pendingReuse = nil
			go func() {
				var outcome Outcome
				var err error
				if reused != nil {
					outcome, err = processReusedReview(ctx, cfg, c, *reused, work, askpass)
				} else {
					outcome, err = processRepository(ctx, cfg, c, repo, version, fingerprint, work, askpass)
				}
				completed <- completion{outcome, err}
			}()
		}
		if !open && active == 0 {
			continue
		}
		incoming := repos
		if !open {
			incoming = nil
		}
		select {
		case <-heartbeat.C:
			cfg.progress("%s: %d finished in batch, %d active, %d queued", c.Name, len(batch.Repositories), active, len(queue))
		case repo, ok := <-incoming:
			if !ok {
				open = false
				continue
			}
			queue = append(queue, repo)
		case result := <-completed:
			active--
			if result.err != nil && failure == nil {
				failure = result.err
				cancel()
			}
			record(result.outcome)
		}
	}
	if err := saveBatch(cfg, batch); err != nil && failure == nil {
		failure = err
	}
	return failure
}
func processRepository(ctx context.Context, cfg Config, c Connection, repo Repository, version, fingerprint, work, askpass string) (Outcome, error) {
	started := time.Now().UTC()
	result := Outcome{Repository: repo, CheckedAt: started, Outcome: "failed"}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	ref := filepath.Join(repositoryDir(repo), "attempts", newID())
	checkout := filepath.Join(work, "checkout-"+newID())
	meta := Metadata{Repository: repo, ScannerVersion: version, InputFingerprint: fingerprint, Configuration: cfg.Deplens, Scope: Scope{Shallow: true}, StartedAt: started, Outcome: "failed"}
	cfg.progress("%s: cloning %s", c.Name, repo.Path)
	err := acquire(ctx, c, repo, checkout, askpass)
	var output any
	if err == nil {
		err = verifyScanner(cfg.Deplens, version)
	}
	if err == nil {
		scanned := time.Now().UTC()
		meta.ScannedAt = &scanned
		result.ScannedAt = &scanned
		cfg.progress("%s: scanning %s with deplens", c.Name, repo.Path)
		output, err = analyze(ctx, cfg, checkout)
		if identityErr := verifyScanner(cfg.Deplens, version); identityErr != nil {
			err = identityErr
		}
		if err == nil {
			after, fingerprintErr := inputFingerprint(cfg.Deplens, version)
			if fingerprintErr != nil || after != fingerprint {
				err = errors.New("scan configuration changed during analysis")
			}
		}
	}
	needsReview := cfg.Review && err == nil
	var cleanupErr error
	if needsReview {
		defer os.RemoveAll(checkout)
	} else {
		cleanupErr = os.RemoveAll(checkout)
		if cleanupErr != nil {
			err = errors.New("cannot remove temporary checkout")
		}
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
	if needsReview {
		performReview(ctx, cfg, checkout, &result)
		cleanupErr = os.RemoveAll(checkout)
		if cleanupErr != nil {
			result.Review = &ReviewOutcome{Status: "failed", Error: "cannot remove temporary review checkout"}
		}
	}
	if cleanupErr != nil {
		return result, errors.New("cannot remove temporary checkout")
	}
	return result, nil
}
func (cfg Config) progress(format string, args ...any) {
	message := redactValue(fmt.Sprintf(format, args...), cfg).(string)
	quoted := strconv.QuoteToGraphic(message)
	fmt.Fprintln(os.Stderr, quoted[1:len(quoted)-1])
}

func (cfg Config) logOutcome(finished int, result Outcome) {
	status := result.Outcome
	if result.Error != "" {
		status += " (" + result.Error + ")"
	}
	if result.Review != nil {
		status += ", review " + result.Review.Status
		if result.Review.Error != "" {
			status += " (" + result.Review.Error + ")"
		}
	}
	cfg.progress("[%d finished] %s: %s: %s", finished, result.Connection, result.Path, status)
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
