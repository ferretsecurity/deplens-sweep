package sweep_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

func TestHardKilledScanStopsOwnedChildrenAndRecoversOnlyOwnedWork(t *testing.T) {
	f := newFixture(t)
	ready := filepath.Join(f.dir, "ready")
	childFile := filepath.Join(f.dir, "child")
	t.Setenv("SWEEP_READY", ready)
	t.Setenv("SWEEP_CHILD", childFile)
	os.WriteFile(f.scanner, []byte("#!/bin/sh\nif [ ! -e \"$SWEEP_READY\" ]; then\n  sleep 60 &\n  echo $! > \"$SWEEP_CHILD\"\n  touch \"$SWEEP_READY\"\n  wait\nfi\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"fixture\",\"sources\":[],\"check_runs\":[],\"findings\":[]}'\n"), 0700)
	cmd, done := recoveryStart(t, f)
	recoveryWaitFile(t, ready)
	childRaw, _ := os.ReadFile(childFile)
	child, _ := strconv.Atoi(strings.TrimSpace(string(childRaw)))
	t.Cleanup(func() {
		if child > 0 {
			syscall.Kill(child, syscall.SIGKILL)
		}
	})
	old := f.summary()
	if old["status"] != "running" {
		t.Fatalf("active batch claims completion: %#v", old)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	recoveryAwait(t, done)
	// The persistent namespace belongs to this results location; unknown siblings
	// and symlinks inside it must survive recovery.
	scopes, _ := filepath.Glob(filepath.Join(f.dir, "work", ".deplens-sweep-*"))
	if len(scopes) != 1 {
		t.Fatalf("no scoped recovery namespace: %v", scopes)
	}
	unknown := filepath.Join(scopes[0], "unrelated")
	os.WriteFile(unknown, []byte("keep"), 0600)
	outside := filepath.Join(f.dir, "unrelated-outside")
	os.Mkdir(outside, 0700)
	os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0600)
	link := filepath.Join(scopes[0], "unrelated-link")
	os.Symlink(outside, link)
	runs, _ := filepath.Glob(filepath.Join(scopes[0], "run-*"))
	foreign := filepath.Join(scopes[0], "run-foreign")
	os.Mkdir(foreign, 0700)
	owner := reuseJSON(t, filepath.Join(runs[0], ".owner.json"))
	owner["results"] = filepath.Join(f.dir, "another-results")
	owner["run"] = "run-foreign"
	ownerBytes, _ := json.Marshal(owner)
	os.WriteFile(filepath.Join(foreign, ".owner.json"), ownerBytes, 0600)
	second := reuseRun(t, f)
	if second["outcome"] != "complete" {
		t.Fatalf("unfinished scan was reused: %#v", second)
	}
	if !recoveryProcessGone(child) {
		t.Fatalf("owned child %d survived recovery", child)
	}
	for _, path := range []string{unknown, link, filepath.Join(outside, "keep"), filepath.Join(foreign, ".owner.json")} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("unrelated work removed: %s", path)
		}
	}
	entries, _ := os.ReadDir(scopes[0])
	if len(entries) != 3 {
		t.Fatalf("owned checkout survived: %v", entries)
	}
	oldSaved := reuseJSON(t, filepath.Join(f.results, "batches", old["batch_id"].(string)+".json"))
	if oldSaved["status"] != "running" || oldSaved["completed_at"] != "0001-01-01T00:00:00Z" {
		t.Fatalf("interrupted batch became complete: %#v", oldSaved)
	}
}

func recoveryProcessGone(pid int) bool {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	parts := strings.Fields(string(raw)[strings.LastIndex(string(raw), ")")+1:])
	return len(parts) > 0 && parts[0] == "Z"
}

func TestInterruptedPersistenceKeepsLatestAndRetriesUnpublishedOutput(t *testing.T) {
	f := newFixture(t)
	output := filepath.Join(f.dir, "output.json")
	complete := `{"schema_version":1,"root":"fixture","sources":[],"check_runs":[],"findings":[]}`
	os.WriteFile(output, []byte(complete), 0600)
	t.Setenv("SWEEP_OUTPUT", output)
	os.WriteFile(f.scanner, []byte("#!/bin/sh\ncat \"$SWEEP_OUTPUT\"\n"), 0700)
	first := reuseRun(t, f)
	latestPath := filepath.Join(f.results, filepath.Dir(filepath.Dir(first["result"].(string))), "latest.json")
	raw, _ := os.ReadFile(f.config)
	os.WriteFile(f.config, append(raw, []byte("  ignore: [replacement]\n")...), 0600)
	large := strings.TrimSuffix(complete, "}") + `,"padding":"` + strings.Repeat("x", 32*1024*1024) + `"}`
	os.WriteFile(output, []byte(large), 0600)
	cmd, done := recoveryStart(t, f)
	t.Cleanup(func() { cmd.Process.Kill() })
	deadline := time.Now().Add(8 * time.Second)
	stopped := false
	for time.Now().Before(deadline) {
		pending, _ := filepath.Glob(filepath.Join(f.results, ".staging", "run-*", "*", ".pending-*"))
		if len(pending) > 0 {
			if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
			// A large public artifact keeps its write observable without test hooks.
			pending, _ = filepath.Glob(filepath.Join(f.results, ".staging", "run-*", "*", ".pending-*"))
			if len(pending) > 0 {
				stopped = true
				break
			}
			cmd.Process.Signal(syscall.SIGCONT)
		}
		select {
		case err := <-done:
			t.Fatalf("publication was not staged: %v", err)
		default:
		}
		time.Sleep(time.Millisecond)
	}
	if !stopped {
		t.Fatal("did not observe staged persistence")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	recoveryAwait(t, done)
	if latest := reuseJSON(t, latestPath); latest["result"] != first["result"] {
		t.Fatalf("interruption replaced latest: %#v", latest)
	}
	os.WriteFile(output, []byte(complete), 0600)
	next := reuseRun(t, f)
	if next["outcome"] != "complete" || next["result"] == first["result"] {
		t.Fatalf("unfinished inventory reused: %#v", next)
	}
	entries, _ := os.ReadDir(filepath.Join(f.results, ".staging"))
	if len(entries) != 0 {
		t.Fatalf("pending state survived recovery: %v", entries)
	}
}

func TestSIGTERMLeavesFailedAttemptAndNextInvocationRetries(t *testing.T) {
	f := newFixture(t)
	ready := filepath.Join(f.dir, "term-ready")
	t.Setenv("SWEEP_READY", ready)
	os.WriteFile(f.scanner, []byte("#!/bin/sh\nif [ ! -e \"$SWEEP_READY\" ]; then touch \"$SWEEP_READY\"; sleep 60; fi\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"fixture\",\"sources\":[],\"check_runs\":[],\"findings\":[]}'\n"), 0700)
	cmd, done := recoveryStart(t, f)
	recoveryWaitFile(t, ready)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := recoveryAwait(t, done); err == nil {
		t.Fatal("terminated batch succeeded")
	}
	summary := f.summary()
	if summary["status"] != "incomplete" || summary["completed_at"] == "0001-01-01T00:00:00Z" {
		t.Fatalf("normal interruption did not finalize incomplete batch: %#v", summary)
	}
	if row := summary["repositories"].([]any)[0].(map[string]any); row["outcome"] != "failed" {
		t.Fatalf("interrupted attempt hidden: %#v", row)
	}
	if row := reuseRun(t, f); row["outcome"] != "complete" {
		t.Fatalf("interrupted scan reused: %#v", row)
	}
	f.assertNoSecretOrCheckout(nil)
}

func TestHardKilledCloneReleasesLockAndRemovesOnlyOwnedCheckout(t *testing.T) {
	f := newFixture(t)
	ready := filepath.Join(f.dir, "clone-ready")
	t.Setenv("SWEEP_READY", ready)
	gitPath := filepath.Join(f.dir, "bin", "git")
	raw, _ := os.ReadFile(gitPath)
	raw = []byte(strings.Replace(string(raw), `if [ "$1" = clone ]; then`, `if [ "$1" = clone ]; then
  if [ ! -e "$SWEEP_READY" ]; then
    for arg in "$@"; do dest="$arg"; done
    mkdir -p "$dest"; echo incomplete > "$dest/partial"
    touch "$SWEEP_READY"
    sleep 60
  fi`, 1))
	os.WriteFile(gitPath, raw, 0700)
	cmd, done := recoveryStart(t, f)
	recoveryWaitFile(t, ready)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	recoveryAwait(t, done)
	if row := reuseRun(t, f); row["outcome"] != "complete" {
		t.Fatalf("interrupted clone reused: %#v", row)
	}
	f.assertNoSecretOrCheckout(nil)
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
	alias := filepath.Join(f.dir, "results-alias")
	if err := os.Symlink(f.results, alias); err != nil {
		t.Fatal(err)
	}
	configBytes, _ := os.ReadFile(f.config)
	os.WriteFile(f.config, []byte(strings.Replace(string(configBytes), "results_dir: "+f.results, "results_dir: "+alias, 1)), 0600)
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
