package sweep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type savedReview struct {
	ReviewOutcome
	InputFingerprint string `json:"input_fingerprint,omitempty"`
	ReportDigest     string `json:"report_digest,omitempty"`
}

func reusableReview(cfg Config, result Outcome) (ReviewOutcome, bool) {
	var saved savedReview
	if !readSavedJSON(cfg.ResultsDir, filepath.Join(result.Result, "review.json"), &saved) || saved.Status != "complete" {
		return ReviewOutcome{}, false
	}
	reportRef := filepath.Join(result.Result, "review.md")
	if saved.Report != filepath.ToSlash(reportRef) || saved.InputFingerprint == "" || saved.ReportDigest == "" {
		return ReviewOutcome{}, false
	}
	scanPath := filepath.Join(cfg.ResultsDir, result.Result, "scan.json")
	fingerprint, err := reviewFingerprint(scanPath)
	if err != nil || fingerprint != saved.InputFingerprint {
		return ReviewOutcome{}, false
	}
	path, ok := savedRegularPath(cfg.ResultsDir, reportRef)
	if !ok {
		return ReviewOutcome{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(raw))) == 0 || digest(raw) != saved.ReportDigest {
		return ReviewOutcome{}, false
	}
	return ReviewOutcome{Status: "reused", Report: saved.Report}, true
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func performReview(ctx context.Context, cfg Config, checkout string, result *Outcome) {
	path := filepath.Join(cfg.ResultsDir, result.Result)
	scanPath := filepath.Join(path, "scan.json")
	saved := savedReview{ReviewOutcome: ReviewOutcome{Status: "failed"}}
	fingerprint, err := reviewFingerprint(scanPath)
	var report []byte
	if err == nil {
		report, err = runReview(ctx, checkout, scanPath)
	}
	if err == nil {
		after, checkErr := reviewFingerprint(scanPath)
		if checkErr != nil || after != fingerprint {
			err = errors.New("review inputs changed during review")
		}
	}
	if err == nil && len(strings.TrimSpace(string(report))) == 0 {
		err = errors.New("review produced an empty report")
	}
	if err == nil {
		redacted := redactValue(string(report), cfg).(string)
		report = []byte(redacted)
		if saveErr := saveBytes(filepath.Join(path, "review.md"), report); saveErr != nil {
			err = errors.New("cannot save review report")
		}
	}
	if err != nil {
		saved.Error = err.Error()
	} else {
		saved.Status = "complete"
		saved.Report = filepath.ToSlash(filepath.Join(result.Result, "review.md"))
		saved.InputFingerprint = fingerprint
		saved.ReportDigest = digest(report)
	}
	if saveErr := saveJSON(filepath.Join(path, "review.json"), redactValue(saved, cfg)); saveErr != nil {
		saved.ReviewOutcome = ReviewOutcome{Status: "failed", Error: "cannot save review metadata"}
	}
	result.Review = &saved.ReviewOutcome
}

func processReusedReview(ctx context.Context, cfg Config, c Connection, result Outcome, work, askpass string) (Outcome, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	checkout := filepath.Join(work, "checkout-"+newID())
	if err := acquire(ctx, c, result.Repository, checkout, askpass); err != nil {
		result.Review = &ReviewOutcome{Status: "failed", Error: err.Error()}
	} else {
		performReview(ctx, cfg, checkout, &result)
	}
	if err := os.RemoveAll(checkout); err != nil {
		result.Review = &ReviewOutcome{Status: "failed", Error: "cannot remove temporary review checkout"}
		return result, errors.New("cannot remove temporary review checkout")
	}
	return result, nil
}
