package sweep

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Outcome struct {
	Repository
	Outcome   string     `json:"outcome"`
	Result    string     `json:"result,omitempty"`
	Error     string     `json:"error,omitempty"`
	CheckedAt time.Time  `json:"checked_at"`
	ScannedAt *time.Time `json:"scanned_at,omitempty"`
}
type Scope struct {
	Shallow    bool `json:"shallow"`
	Submodules bool `json:"submodules"`
	GitLFS     bool `json:"git_lfs"`
}
type Metadata struct {
	Repository
	ScannerVersion   string     `json:"scanner_version"`
	InputFingerprint string     `json:"input_fingerprint"`
	Configuration    ScanConfig `json:"configuration"`
	Scope            Scope      `json:"scope"`
	StartedAt        time.Time  `json:"started_at"`
	ScannedAt        *time.Time `json:"scanned_at,omitempty"`
	CompletedAt      time.Time  `json:"completed_at"`
	Outcome          string     `json:"outcome"`
	Error            string     `json:"error,omitempty"`
}
type Batch struct {
	SchemaVersion     int            `json:"schema_version"`
	ID                string         `json:"batch_id"`
	StartedAt         time.Time      `json:"started_at"`
	CompletedAt       time.Time      `json:"completed_at,omitempty"`
	Selections        []Connection   `json:"selections"`
	Repositories      []Outcome      `json:"repositories"`
	Counts            map[string]int `json:"counts"`
	DiscoveryComplete bool           `json:"discovery_complete"`
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(b)
}
func repositoryDir(repo Repository) string {
	sum := sha256.Sum256([]byte(repo.Provider + "\x00" + repo.ServerURL + "\x00" + repo.ID))
	return filepath.Join("connections", repo.Connection, hex.EncodeToString(sum[:16]))
}
func saveJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".pending-")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func saveBatch(cfg Config, batch *Batch) error {
	batch.Counts = map[string]int{"complete": 0, "failed": 0, "empty": 0, "reused": 0, "unprocessed": 0}
	for _, r := range batch.Repositories {
		batch.Counts[r.Outcome]++
	}
	if err := saveJSON(filepath.Join(cfg.ResultsDir, "batches", batch.ID+".json"), redactValue(batch, cfg)); err != nil {
		return fmt.Errorf("save batch summary: %w", err)
	}
	return nil
}
