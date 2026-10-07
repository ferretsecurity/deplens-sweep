package sweep_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func recoveryWaitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process did not reach %s", path)
}

func recoveryStart(t *testing.T, f *fixture) (*exec.Cmd, <-chan error) {
	t.Helper()
	out, err := os.CreateTemp(f.dir, "process-output-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { out.Close() })
	cmd := exec.Command(binary, "--config", f.config)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return cmd, done
}

func recoveryAwait(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("CLI did not finish")
		return nil
	}
}

func TestResultsLockRejectsCompetingInvocationAndAllowsNextRun(t *testing.T) {
	f := newFixture(t)
	ready, release := filepath.Join(f.dir, "ready"), filepath.Join(f.dir, "release")
	t.Setenv("SWEEP_READY", ready)
	t.Setenv("SWEEP_RELEASE", release)
	os.WriteFile(f.scanner, []byte("#!/bin/sh\ntouch \"$SWEEP_READY\"\nwhile [ ! -e \"$SWEEP_RELEASE\" ]; do sleep 0.02; done\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"fixture\",\"sources\":[],\"check_runs\":[],\"findings\":[]}'\n"), 0700)
	_, done := recoveryStart(t, f)
	t.Cleanup(func() { os.WriteFile(release, nil, 0600) })
	recoveryWaitFile(t, ready)
	before, _ := filepath.Glob(filepath.Join(f.results, "batches", "*.json"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "--config", f.config).CombinedOutput()
	os.WriteFile(release, nil, 0600)
	if firstErr := recoveryAwait(t, done); firstErr != nil {
		t.Fatalf("first batch: %v", firstErr)
	}
	if err == nil || !strings.Contains(string(out), "results directory is locked") {
		t.Fatalf("competing run not clearly rejected: %v %s", err, out)
	}
	after, _ := filepath.Glob(filepath.Join(f.results, "batches", "*.json"))
	if len(after) != len(before) {
		t.Fatalf("competing invocation mutated batch state: %v", after)
	}
	if row := reuseRun(t, f); row["outcome"] != "reused" {
		t.Fatalf("released lock did not allow reuse: %#v", row)
	}
}
