package sweep_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func (f *fixture) workerProjects(n int) {
	paths := ""
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("team/project%d", i)
		f.projects[name] = map[string]any{"id": 100 + i, "path_with_namespace": name, "default_branch": "main", "http_url_to_repo": strings.Replace(f.server.URL, "http:", "https:", 1) + "/" + name + ".git"}
		paths += "  - " + name + "\n"
	}
	f.writeConfig(paths, "")
}
func (f *fixture) executionConfig(extra string) {
	data, err := os.ReadFile(f.config)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = os.WriteFile(f.config, append(data, []byte(extra)...), 0600); err != nil {
		f.t.Fatal(err)
	}
}
func waitForFiles(t *testing.T, pattern string, count int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		paths, _ := filepath.Glob(pattern)
		if len(paths) == count {
			return paths
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("did not observe %d files for %s", count, pattern)
	return nil
}
func TestWorkerLimitAllowsParallelScansAndSavesEveryOutcome(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		workers      int
	}{{"default", "", 3}, {"configured", "workers: 2\n", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.workerProjects(tc.workers + 1)
			f.executionConfig(tc.config)
			t.Setenv("SWEEP_MARKERS", filepath.Join(f.dir, "markers"))
			os.MkdirAll(os.Getenv("SWEEP_MARKERS"), 0700)
			os.WriteFile(f.scanner, []byte(`#!/bin/sh
for arg in "$@"; do checkout="$arg"; done
name=$(basename "$checkout")
printf '%s' "$checkout" > "$SWEEP_MARKERS/$name.started"
while [ ! -f "$SWEEP_MARKERS/release" ]; do sleep 0.01; done
printf '%s\n' '{"schema_version":1,"root":"fixture","sources":[],"check_runs":[],"findings":[]}'
`), 0700)
			f.syncServer()
			cmd := exec.Command(binary, "--config", f.config)
			done := make(chan error, 1)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				os.WriteFile(filepath.Join(os.Getenv("SWEEP_MARKERS"), "release"), nil, 0600)
				cmd.Process.Kill()
			})
			go func() { done <- cmd.Wait() }()
			waitForFiles(t, filepath.Join(os.Getenv("SWEEP_MARKERS"), "*.started"), tc.workers)
			// The final repository cannot enter analysis while every worker is blocked.
			time.Sleep(80 * time.Millisecond)
			paths, _ := filepath.Glob(filepath.Join(os.Getenv("SWEEP_MARKERS"), "*.started"))
			if len(paths) != tc.workers {
				t.Fatalf("active scans=%d, limit=%d", len(paths), tc.workers)
			}
			os.WriteFile(filepath.Join(os.Getenv("SWEEP_MARKERS"), "release"), nil, 0600)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("batch did not finish")
			}
			counts := f.summary()["counts"].(map[string]any)
			if counts["complete"] != float64(tc.workers+1) {
				t.Fatalf("counts=%#v", counts)
			}
			f.assertNoSecretOrCheckout(nil)
		})
	}
}

func TestWorkerCleansCheckoutAndSavesOutcomeBeforeNextRepository(t *testing.T) {
	f := newFixture(t)
	f.workerProjects(2)
	f.executionConfig("workers: 1\n")
	t.Setenv("SWEEP_MARKERS", filepath.Join(f.dir, "previous"))
	t.Setenv("SWEEP_RESULTS", f.results)
	os.WriteFile(f.scanner, []byte(`#!/bin/sh
for arg in "$@"; do checkout="$arg"; done
if [ -f "$SWEEP_MARKERS" ]; then
 previous=$(cat "$SWEEP_MARKERS")
 [ ! -e "$previous" ] || exit 81
 find "$SWEEP_RESULTS/connections" -name metadata.json | grep -q . || exit 82
fi
printf '%s' "$checkout" > "$SWEEP_MARKERS"
printf '%s\n' '{"schema_version":1,"root":"fixture","sources":[],"check_runs":[],"findings":[]}'
`), 0700)
	out, err := f.run()
	if err != nil {
		t.Fatalf("worker reused before cleanup/save: %v %s", err, out)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestTimeoutTerminatesScannerChildrenAndContinues(t *testing.T) {
	f := newFixture(t)
	f.workerProjects(2)
	f.executionConfig("workers: 1\ntimeout: 200ms\n")
	markers := filepath.Join(f.dir, "markers")
	os.MkdirAll(markers, 0700)
	t.Setenv("SWEEP_MARKERS", markers)
	os.WriteFile(f.scanner, []byte(`#!/bin/sh
if mkdir "$SWEEP_MARKERS/first" 2>/dev/null; then
 echo $$ > "$SWEEP_MARKERS/scanner.pid"
 /bin/sh -c 'echo $$ > "$SWEEP_MARKERS/child.pid"; while [ ! -f "$SWEEP_MARKERS/release" ]; do sleep 0.01; done; touch "$SWEEP_MARKERS/child-finished"' &
 wait
fi
printf '%s\n' '{"schema_version":1,"root":"fixture","sources":[],"check_runs":[],"findings":[]}'
`), 0700)
	fallback := time.AfterFunc(3*time.Second, func() { os.WriteFile(filepath.Join(markers, "release"), nil, 0600) })
	defer fallback.Stop()
	out, err := f.run()
	if err == nil {
		t.Fatalf("timeout accepted: %s", out)
	}
	counts := f.summary()["counts"].(map[string]any)
	if counts["failed"] != float64(1) || counts["complete"] != float64(1) {
		t.Fatalf("counts=%#v", counts)
	}
	outcomes := f.summary()["repositories"].([]any)
	if !strings.Contains(outcomes[0].(map[string]any)["error"].(string), "timeout") {
		t.Fatalf("timeout not reported: %#v", outcomes)
	}
	for _, name := range []string{"scanner.pid", "child.pid"} {
		data, err := os.ReadFile(filepath.Join(markers, name))
		if err != nil {
			t.Fatal(err)
		}
		pid := strings.TrimSpace(string(data))
		status, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
		if err == nil && !strings.Contains(string(status), ") Z ") {
			t.Errorf("%s still running: %s", name, status)
		}
	}
	os.WriteFile(filepath.Join(markers, "release"), nil, 0600)
	if _, err = os.Stat(filepath.Join(markers, "child-finished")); err == nil {
		t.Fatal("timed-out child continued work")
	}
	f.assertNoSecretOrCheckout(out)
}

func TestTimeoutIsOneBudgetForCloneAndScan(t *testing.T) {
	f := newFixture(t)
	f.executionConfig("timeout: 250ms\n")
	gitPath := filepath.Join(f.dir, "bin", "git")
	script, _ := os.ReadFile(gitPath)
	script = []byte(strings.Replace(string(script), "if [ \"$1\" = clone ]; then", "if [ \"$1\" = clone ]; then\n  sleep 0.15", 1))
	os.WriteFile(gitPath, script, 0700)
	scan, _ := os.ReadFile(f.scanner)
	scan = []byte(strings.Replace(string(scan), "#!/bin/sh\n", "#!/bin/sh\nsleep 0.15\n", 1))
	os.WriteFile(f.scanner, scan, 0700)
	out, err := f.run()
	if err == nil {
		t.Fatalf("clone and scan received separate budgets: %s", out)
	}
	result := f.summary()["repositories"].([]any)[0].(map[string]any)
	if !strings.Contains(result["error"].(string), "timeout") {
		t.Fatalf("outcome=%#v", result)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestTimeoutStopsCloneChildrenAndRemovesPartialCheckout(t *testing.T) {
	f := newFixture(t)
	f.executionConfig("timeout: 200ms\n")
	markers := filepath.Join(f.dir, "markers")
	os.MkdirAll(markers, 0700)
	t.Setenv("SWEEP_MARKERS", markers)
	gitPath := filepath.Join(f.dir, "bin", "git")
	script, _ := os.ReadFile(gitPath)
	script = []byte(strings.Replace(string(script), "if [ \"$1\" = clone ]; then", `if [ "$1" = clone ]; then
  for arg in "$@"; do dest="$arg"; done
  mkdir -p "$dest"
  /bin/sh -c 'echo $$ > "$SWEEP_MARKERS/clone-child.pid"; while [ ! -f "$SWEEP_MARKERS/release" ]; do sleep 0.01; done' &
  wait
  exit 0`, 1))
	os.WriteFile(gitPath, script, 0700)
	fallback := time.AfterFunc(3*time.Second, func() { os.WriteFile(filepath.Join(markers, "release"), nil, 0600) })
	defer fallback.Stop()
	out, err := f.run()
	if err == nil {
		t.Fatalf("clone timeout accepted: %s", out)
	}
	result := f.summary()["repositories"].([]any)[0].(map[string]any)
	if !strings.Contains(result["error"].(string), "timeout") {
		t.Fatalf("outcome=%#v", result)
	}
	pid, err := os.ReadFile(filepath.Join(markers, "clone-child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	status, err := os.ReadFile(filepath.Join("/proc", strings.TrimSpace(string(pid)), "stat"))
	if err == nil && !strings.Contains(string(status), ") Z ") {
		t.Fatalf("clone child survived: %s", status)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestExitedScannerCannotLeaveChildWorkRunning(t *testing.T) {
	f := newFixture(t)
	markers := filepath.Join(f.dir, "markers")
	os.MkdirAll(markers, 0700)
	t.Setenv("SWEEP_MARKERS", markers)
	os.WriteFile(f.scanner, []byte(`#!/bin/sh
/bin/sh -c 'echo $$ > "$SWEEP_MARKERS/child.pid"; while [ ! -f "$SWEEP_MARKERS/release" ]; do sleep 0.01; done' &
while [ ! -f "$SWEEP_MARKERS/child.pid" ]; do sleep 0.01; done
printf '%s\n' '{"schema_version":1,"root":"fixture","sources":[],"check_runs":[],"findings":[]}'
`), 0700)
	fallback := time.AfterFunc(3*time.Second, func() { os.WriteFile(filepath.Join(markers, "release"), nil, 0600) })
	defer fallback.Stop()
	out, _ := f.run()
	pid, err := os.ReadFile(filepath.Join(markers, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	status, err := os.ReadFile(filepath.Join("/proc", strings.TrimSpace(string(pid)), "stat"))
	if err == nil && !strings.Contains(string(status), ") Z ") {
		t.Fatalf("exited scanner left work running: %s", status)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestWorkerAndTimeoutSettingsMustBePositive(t *testing.T) {
	for _, setting := range []string{"workers: -1\n", "workers: 0\n", "timeout: -1s\n", "timeout: 0s\n", "timeout: invalid\n"} {
		t.Run(strings.TrimSpace(setting), func(t *testing.T) {
			f := newFixture(t)
			f.executionConfig(setting)
			out, err := f.run()
			if err == nil {
				t.Fatalf("invalid setting accepted: %s", out)
			}
			if !strings.Contains(string(out), "workers") && !strings.Contains(string(out), "timeout") && !strings.Contains(strings.ToLower(string(out)), "duration") {
				t.Fatalf("unclear error: %s", out)
			}
		})
	}
}

func TestCleanupFailureStopsWorkerBeforeItAcceptsAnotherRepository(t *testing.T) {
	f := newFixture(t)
	f.workerProjects(2)
	f.executionConfig("workers: 1\n")
	os.WriteFile(f.scanner, []byte(`#!/bin/sh
for arg in "$@"; do checkout="$arg"; done
mkdir "$checkout/blocked"
touch "$checkout/blocked/file"
chmod 000 "$checkout/blocked"
printf '%s\n' '{"schema_version":1,"root":"fixture","sources":[],"check_runs":[],"findings":[]}'
`), 0700)
	t.Cleanup(func() {
		filepath.Walk(filepath.Join(f.dir, "work"), func(path string, info os.FileInfo, err error) error {
			if info != nil && info.IsDir() {
				os.Chmod(path, 0700)
			}
			return nil
		})
	})
	out, err := f.run()
	if err == nil {
		t.Fatalf("cleanup failure accepted: %s", out)
	}
	counts := f.summary()["counts"].(map[string]any)
	if counts["failed"] != float64(1) || counts["unprocessed"] != float64(1) {
		t.Fatalf("worker accepted new work with a checkout remaining: %#v", counts)
	}
}

func TestTerminationCancelsActiveWorkAndMarksPendingRepositories(t *testing.T) {
	f := newFixture(t)
	f.workerProjects(3)
	f.executionConfig("workers: 1\n")
	markers := filepath.Join(f.dir, "markers")
	os.MkdirAll(markers, 0700)
	t.Setenv("SWEEP_MARKERS", markers)
	os.WriteFile(f.scanner, []byte(`#!/bin/sh
touch "$SWEEP_MARKERS/started"
while [ ! -f "$SWEEP_MARKERS/release" ]; do sleep 0.01; done
`), 0700)
	f.syncServer()
	cmd := exec.Command(binary, "--config", f.config)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.WriteFile(filepath.Join(markers, "release"), nil, 0600); cmd.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waitForFiles(t, filepath.Join(markers, "started"), 1)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled batch exited successfully")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled batch did not stop")
	}
	counts := f.summary()["counts"].(map[string]any)
	if counts["failed"] != float64(1) || counts["unprocessed"] != float64(2) {
		t.Fatalf("canceled outcomes=%#v", counts)
	}
	f.assertNoSecretOrCheckout(nil)
}
