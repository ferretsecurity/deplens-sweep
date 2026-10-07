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
	batch = Batch{SchemaVersion: 1, ID: newID(), StartedAt: time.Now().UTC(), Selections: cfg.Connections, Repositories: []Outcome{}, DiscoveryComplete: true}
	version, err := scannerIdentity(cfg.Deplens)
	if err != nil {
		return batch, err
	}
	if err = os.MkdirAll(cfg.ResultsDir, 0700); err != nil {
		return batch, errors.New("cannot create results directory")
	}
	if cfg.TempDir != "" {
		if err = os.MkdirAll(cfg.TempDir, 0700); err != nil {
			return batch, errors.New("cannot create temporary checkout directory")
		}
	}
	work, err := os.MkdirTemp(cfg.TempDir, "deplens-sweep-")
	if err != nil {
		return batch, errors.New("cannot create temporary workspace")
	}
	defer os.RemoveAll(work)
	askpass, err := writeAskpass(work)
	if err != nil {
		return batch, errors.New("cannot prepare Git authentication")
	}
	if err = saveBatch(cfg, &batch); err != nil {
		return batch, err
	}
	for _, c := range cfg.Connections {
		repos, failures := discoverGitLab(ctx, c)
		if len(failures) > 0 {
			batch.DiscoveryComplete = false
		}
		for _, failure := range failures {
			batch.Repositories = append(batch.Repositories, failure)
			if err = saveBatch(cfg, &batch); err != nil {
				return batch, err
			}
		}
		for _, repo := range repos {
			result, processErr := processRepository(ctx, cfg, c, repo, version, work, askpass)
			batch.Repositories = append(batch.Repositories, result)
			if err = saveBatch(cfg, &batch); err != nil {
				return batch, err
			}
			if processErr != nil {
				return batch, processErr
			}
		}
	}
	batch.CompletedAt = time.Now().UTC()
	if err = saveBatch(cfg, &batch); err != nil {
		return batch, err
	}
	if !batch.DiscoveryComplete || batch.Counts["failed"] > 0 {
		return batch, errors.New("batch is incomplete; see saved repository outcomes")
	}
	return batch, nil
}
func processRepository(ctx context.Context, cfg Config, c Connection, repo Repository, version, work, askpass string) (Outcome, error) {
	started := time.Now().UTC()
	result := Outcome{Repository: repo, CheckedAt: started, Outcome: "empty"}
	if repo.Empty {
		return result, nil
	}
	ref := filepath.Join(repositoryDir(repo), "attempts", newID())
	checkout := filepath.Join(work, "checkout")
	meta := Metadata{Repository: repo, ScannerVersion: version, Configuration: cfg.Deplens, Scope: Scope{Shallow: true}, StartedAt: started, Outcome: "failed"}
	err := acquire(ctx, c, repo, checkout, askpass)
	var output any
	if err == nil {
		output, err = analyze(ctx, cfg, checkout)
	}
	cleanupErr := os.RemoveAll(checkout)
	if cleanupErr != nil && err == nil {
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
	if output != nil {
		if saveErr := saveJSON(filepath.Join(cfg.ResultsDir, ref, "scan.json"), redactValue(output, cfg)); saveErr != nil {
			return storageFailure("cannot save scan output")
		}
	}
	if saveErr := saveJSON(filepath.Join(cfg.ResultsDir, ref, "metadata.json"), redactValue(meta, cfg)); saveErr != nil {
		return storageFailure("cannot save scan metadata")
	}
	result.Result = filepath.ToSlash(ref)
	if result.Outcome == "complete" {
		if saveErr := saveJSON(filepath.Join(cfg.ResultsDir, repositoryDir(repo), "latest.json"), redactValue(result, cfg)); saveErr != nil {
			return storageFailure("cannot publish complete result")
		}
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
