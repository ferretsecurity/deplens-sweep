package sweep

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func inputFingerprint(cfg ScanConfig, version string) (string, error) {
	// Rule paths are only locators. Hash their bytes so an edit in place invalidates
	// reuse, while moving the same rule file does not change effective inputs.
	contents := func(path string) (string, error) {
		if path == "" {
			return "", nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", errors.New("cannot read custom rules")
		}
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:]), nil
	}
	effective := cfg
	var err error
	effective.Rules, err = contents(cfg.Rules)
	if err != nil {
		return "", err
	}
	effective.ExtendRules = make([]string, len(cfg.ExtendRules))
	for i, path := range cfg.ExtendRules {
		effective.ExtendRules[i], err = contents(path)
		if err != nil {
			return "", err
		}
	}
	// Use the scanner's actual argument encoding for flags and effective defaults.
	raw, _ := json.Marshal(struct {
		Version   string
		Arguments []string
		Scope     Scope
	}{version, scanArguments(effective, ""), Scope{Shallow: true}})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func prepareRepository(cfg Config, repo Repository, version string) (string, Outcome, bool, error) {
	if err := verifyScanner(cfg.Deplens, version); err != nil {
		return "", Outcome{}, false, err
	}
	fingerprint, err := inputFingerprint(cfg.Deplens, version)
	if err != nil {
		return "", Outcome{}, false, err
	}
	result, ok, err := findReusable(cfg, repo, fingerprint)
	return fingerprint, result, ok, err
}

func findReusable(cfg Config, repo Repository, fingerprint string) (Outcome, bool, error) {
	dir := filepath.Join(cfg.ResultsDir, repositoryDir(repo))
	// Every retained complete attempt is eligible, including one older than latest.
	// This matters when inputs return to a previously scanned commit or configuration.
	entries, err := os.ReadDir(filepath.Join(dir, "attempts"))
	if err != nil && !os.IsNotExist(err) {
		return Outcome{}, false, errors.New("cannot inspect saved repository history")
	}
	var latest *Outcome
	var reused Outcome
	found := false
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		ref := filepath.Join(repositoryDir(repo), "attempts", entry.Name())
		var meta Metadata
		if !readSavedJSON(cfg.ResultsDir, filepath.Join(ref, "metadata.json"), &meta) {
			continue
		}
		if meta.Outcome != "complete" || meta.InputFingerprint == "" || meta.ScannerVersion == "" ||
			meta.Connection != repo.Connection || meta.Provider != repo.Provider ||
			meta.ServerURL != repo.ServerURL || meta.ID != repo.ID || !validRevision(meta.Revision) ||
			meta.StartedAt.IsZero() || meta.ScannedAt == nil || meta.ScannedAt.Before(meta.StartedAt) || meta.CompletedAt.Before(*meta.ScannedAt) {
			continue
		}
		matches := meta.InputFingerprint == fingerprint && meta.Revision == repo.Revision
		if latest != nil && !matches {
			continue
		}
		var output any
		if !readSavedJSON(cfg.ResultsDir, filepath.Join(ref, "scan.json"), &output) || completeAnalysis(output) != nil {
			continue
		}
		scanned := *meta.ScannedAt
		if latest == nil {
			latest = &Outcome{Repository: meta.Repository, Outcome: "complete", Result: filepath.ToSlash(ref), CheckedAt: meta.StartedAt, ScannedAt: &scanned}
		}
		if matches {
			reused = Outcome{Repository: repo, Outcome: "reused", Result: filepath.ToSlash(ref), CheckedAt: time.Now().UTC(), ScannedAt: &scanned}
			found = true
			break
		}
	}
	if err := repairLatest(cfg, repo, latest); err != nil {
		return Outcome{}, false, err
	}
	return reused, found, nil
}

func repairLatest(cfg Config, repo Repository, latest *Outcome) error {
	ref := filepath.Join(repositoryDir(repo), "latest.json")
	path := filepath.Join(cfg.ResultsDir, ref)
	parent := cfg.ResultsDir
	for _, part := range strings.Split(repositoryDir(repo), string(filepath.Separator)) {
		parent = filepath.Join(parent, part)
		info, err := os.Lstat(parent)
		if os.IsNotExist(err) && latest == nil {
			return nil
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("cannot repair latest complete reference through invalid history path")
		}
	}
	if latest == nil {
		// Invalid or missing artifacts cannot keep advertising a complete result.
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil || info.IsDir() {
			return errors.New("cannot repair latest complete reference")
		}
		if err := os.Remove(path); err != nil {
			return errors.New("cannot repair latest complete reference")
		}
		return nil
	}
	var current Outcome
	if readSavedJSON(cfg.ResultsDir, ref, &current) && current.Result == latest.Result && current.Outcome == "complete" &&
		current.Connection == latest.Connection && current.Provider == latest.Provider && current.ServerURL == latest.ServerURL && current.ID == latest.ID &&
		current.Revision == latest.Revision && current.Path == latest.Path && current.DefaultBranch == latest.DefaultBranch &&
		current.CheckedAt.Equal(latest.CheckedAt) && current.ScannedAt != nil && current.ScannedAt.Equal(*latest.ScannedAt) {
		return nil
	}
	if err := saveJSON(path, redactValue(latest, cfg)); err != nil {
		return errors.New("cannot repair latest complete reference")
	}
	return nil
}

func readSavedJSON(root, ref string, value any) bool {
	// Reject links at every component, including attempt directories. Results may
	// be damaged or edited; never let a history entry read files outside its root.
	if filepath.IsAbs(ref) || filepath.Clean(ref) != ref {
		return false
	}
	path := root
	for _, part := range strings.Split(ref, string(filepath.Separator)) {
		if part == ".." || part == "." || part == "" {
			return false
		}
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	raw, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(raw, value) == nil
}
