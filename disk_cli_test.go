package sweep_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func (f *fixture) diskReserve(bytes uint64) {
	f.t.Helper()
	data, err := os.ReadFile(f.config)
	if err != nil {
		f.t.Fatal(err)
	}
	setting := fmt.Sprintf("min_free_bytes: %d\n", bytes)
	if strings.Contains(string(data), "min_free_bytes: 0\n") {
		data = []byte(strings.Replace(string(data), "min_free_bytes: 0\n", setting, 1))
	} else {
		data = append(data, []byte(setting)...)
	}
	if err = os.WriteFile(f.config, data, 0600); err != nil {
		f.t.Fatal(err)
	}
}

func TestDiskReserveStopsClonesAndRecordsUnprocessedRepositories(t *testing.T) {
	f := newFixture(t)
	f.workerProjects(2)
	f.diskReserve(^uint64(0))
	out, err := f.run()
	if err == nil {
		t.Fatalf("low-space batch accepted: %s", out)
	}
	summary := f.summary()
	counts := summary["counts"].(map[string]any)
	if counts["unprocessed"] != float64(2) || counts["failed"] != float64(0) || summary["status"] != "incomplete" {
		t.Fatalf("outcomes=%#v", summary)
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if strings.Contains(string(log), "clone\n") {
		t.Fatalf("clone admitted below reserve: %s", log)
	}
	for _, raw := range summary["repositories"].([]any) {
		if !strings.Contains(raw.(map[string]any)["error"].(string), "free space") {
			t.Fatalf("low space unclear: %#v", raw)
		}
	}
	f.assertNoSecretOrCheckout(out)
}

func TestLowSpaceStillAllowsReusableAndEmptyRepositories(t *testing.T) {
	f := newFixture(t)
	out, err := f.run()
	if err != nil {
		t.Fatalf("initial inventory: %v %s", err, out)
	}
	first := f.summary()["repositories"].([]any)[0].(map[string]any)["result"].(string)
	f.projects["team/new"] = map[string]any{"id": 43, "path_with_namespace": "team/new", "default_branch": "main", "http_url_to_repo": strings.Replace(f.server.URL, "http:", "https:", 1) + "/team/new.git"}
	f.projects["team/empty"] = map[string]any{"id": 44, "path_with_namespace": "team/empty", "empty_repo": true}
	f.writeConfig("  - team/new\n  - team/project\n  - team/empty\n", "")
	f.diskReserve(^uint64(0))
	os.WriteFile(filepath.Join(f.dir, "git.log"), nil, 0600)
	out, err = f.run()
	if err == nil {
		t.Fatalf("unprocessed repository accepted: %s", out)
	}
	summary := f.summary()
	counts := summary["counts"].(map[string]any)
	if counts["unprocessed"] != float64(1) || counts["reused"] != float64(1) || counts["empty"] != float64(1) || counts["failed"] != float64(0) {
		t.Fatalf("low space blocked reusable/empty outcomes: %#v", summary)
	}
	for _, raw := range summary["repositories"].([]any) {
		result := raw.(map[string]any)
		if result["outcome"] == "reused" && result["result"] != first {
			t.Fatalf("reuse lost history: %#v", result)
		}
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if strings.Contains(string(log), "clone\n") {
		t.Fatalf("clone admitted below reserve: %s", log)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestDiskInspectionFailureDoesNotAdmitMoreClones(t *testing.T) {
	f := newFixture(t)
	f.workerProjects(2)
	f.executionConfig("workers: 1\n")
	os.WriteFile(f.scanner, []byte(`#!/bin/sh
for arg in "$@"; do checkout="$arg"; done
rm -rf "$(dirname "$checkout")"
printf '%s\n' '{"schema_version":1,"root":"fixture","sources":[],"check_runs":[],"findings":[]}'
`), 0700)
	out, err := f.run()
	if err == nil {
		t.Fatalf("disk inspection failure accepted: %s", out)
	}
	summary := f.summary()
	counts := summary["counts"].(map[string]any)
	if counts["complete"] != float64(1) || counts["unprocessed"] != float64(1) || counts["failed"] != float64(0) {
		t.Fatalf("outcomes=%#v", summary)
	}
	for _, raw := range summary["repositories"].([]any) {
		result := raw.(map[string]any)
		if result["outcome"] == "unprocessed" && !strings.Contains(result["error"].(string), "cannot inspect free space") {
			t.Fatalf("inspection failure hidden: %#v", result)
		}
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if strings.Count(string(log), "clone\n") != 1 {
		t.Fatalf("clone admitted after inspection failure: %s", log)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestActiveDiskFailurePreservesEarlierHistoryAndContinues(t *testing.T) {
	f := newFixture(t)
	out, err := f.run()
	if err != nil {
		t.Fatalf("initial result: %v %s", err, out)
	}
	previous := f.summary()["repositories"].([]any)[0].(map[string]any)["result"].(string)
	f.workerProjects(2)
	f.projects["team/project0"]["id"] = 42
	scanner, _ := os.ReadFile(f.scanner)
	os.WriteFile(f.scanner, append(scanner, []byte("# changed scanner identity\n")...), 0700)
	f.executionConfig("workers: 1\n")
	gitPath := filepath.Join(f.dir, "bin", "git")
	script, _ := os.ReadFile(gitPath)
	script = []byte(strings.Replace(string(script), "if [ \"$1\" = clone ]; then", `if [ "$1" = clone ]; then
  case "$*" in *project0.git*) for arg in "$@"; do dest="$arg"; done; mkdir -p "$dest"; echo 'fatal: No space left on device test-secret' >&2; exit 3 ;; esac`, 1))
	os.WriteFile(gitPath, script, 0700)
	out, err = f.run()
	if err == nil {
		t.Fatalf("active disk failure accepted: %s", out)
	}
	counts := f.summary()["counts"].(map[string]any)
	if counts["complete"] != float64(1) || counts["failed"] != float64(1) || counts["unprocessed"] != float64(0) {
		t.Fatalf("outcomes=%#v", counts)
	}
	if _, err = os.Stat(filepath.Join(f.results, previous, "scan.json")); err != nil {
		t.Fatal("prior inventory removed")
	}
	var latest map[string]any
	data, _ := os.ReadFile(filepath.Join(f.results, filepath.Dir(filepath.Dir(previous)), "latest.json"))
	json.Unmarshal(data, &latest)
	if latest["result"] != previous {
		t.Fatalf("active disk failure replaced latest complete: %#v", latest)
	}
	f.assertNoSecretOrCheckout(out)
}

func availableDiskBytes(t *testing.T, path string) uint64 {
	t.Helper()
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		t.Fatal(err)
	}
	return stats.Bavail * uint64(stats.Bsize)
}
func waitForCheckoutRemoval(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("checkout was not cleaned: %s", path)
}

func TestDiskAdmissionWaitsForActiveCheckoutCleanup(t *testing.T) {
	f := newFixture(t)
	f.workerProjects(3)
	f.executionConfig("workers: 2\n")
	markers := filepath.Join(f.dir, "markers")
	os.MkdirAll(markers, 0700)
	t.Setenv("SWEEP_MARKERS", markers)
	gitPath := filepath.Join(f.dir, "bin", "git")
	script, _ := os.ReadFile(gitPath)
	script = []byte(strings.Replace(string(script), `/usr/bin/git clone --no-checkout --depth 1 --single-branch --branch main "file://$SWEEP_ORIGIN" "$dest"`, `/usr/bin/git clone --no-checkout --depth 1 --single-branch --branch main "file://$SWEEP_ORIGIN" "$dest"
  cloned=$?
  [ "$cloned" -eq 0 ] || exit "$cloned"
  case "$*" in
  *project0.git*) printf project0 > "$dest/.fixture-project" ;;
  *project1.git*) printf project1 > "$dest/.fixture-project" ;;
  *project2.git*) printf project2 > "$dest/.fixture-project" ;;
  esac`, 1))
	os.WriteFile(gitPath, script, 0700)
	os.WriteFile(f.scanner, []byte(`#!/bin/sh
for arg in "$@"; do checkout="$arg"; done
name=$(cat "$checkout/.fixture-project")
if [ "$name" = project1 ]; then
 /usr/bin/dd if=/dev/zero of="$checkout/ballast" bs=1048576 count=16 status=none || exit 81
fi
printf '%s' "$checkout" > "$SWEEP_MARKERS/$name.started"
while [ ! -f "$SWEEP_MARKERS/$name.release" ]; do sleep 0.01; done
printf '%s\n' '{"schema_version":1,"root":"fixture","sources":[],"check_runs":[],"findings":[]}'
`), 0700)
	free := availableDiskBytes(t, f.dir)
	if free < 1024*1024*1024 {
		t.Skip("need 1 GiB free to safely allocate a 16 MiB test ballast")
	}
	reserve := free - 8*1024*1024
	f.diskReserve(reserve)
	f.syncServer()
	cmd := exec.Command(binary, "--config", f.config)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() { done <- cmd.Wait(); close(finished) }()
	t.Cleanup(func() {
		for _, name := range []string{"project0", "project1", "project2"} {
			os.WriteFile(filepath.Join(markers, name+".release"), nil, 0600)
		}
		select {
		case <-finished:
			return
		default:
		}
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-finished:
		case <-time.After(time.Second):
			cmd.Process.Kill()
		}
	})
	waitForFiles(t, filepath.Join(markers, "*.started"), 2)
	if freeNow := availableDiskBytes(t, f.dir); freeNow >= reserve {
		t.Skip("host free space changed enough to remove the controlled shortage")
	}
	first, _ := os.ReadFile(filepath.Join(markers, "project0.started"))
	second, _ := os.ReadFile(filepath.Join(markers, "project1.started"))
	os.WriteFile(filepath.Join(markers, "project0.release"), nil, 0600)
	waitForCheckoutRemoval(t, string(first))
	time.Sleep(80 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(markers, "project2.started")); err == nil {
		t.Fatal("third repository started while active ballast kept space below reserve")
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if strings.Count(string(log), "clone\n") != 2 {
		t.Fatalf("clone started below reserve: %s", log)
	}
	os.WriteFile(filepath.Join(markers, "project1.release"), nil, 0600)
	waitForFiles(t, filepath.Join(markers, "project2.started"), 1)
	if _, err := os.Stat(string(second)); !os.IsNotExist(err) {
		t.Fatalf("third clone started before checkout cleanup: %v", err)
	}
	os.WriteFile(filepath.Join(markers, "project2.release"), nil, 0600)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recovered batch: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("batch did not continue after cleanup")
	}
	counts := f.summary()["counts"].(map[string]any)
	if counts["complete"] != float64(3) || counts["unprocessed"] != float64(0) {
		t.Fatalf("recovered outcomes=%#v", counts)
	}
	f.assertNoSecretOrCheckout(nil)
}

func TestPersistentDiskShortagePreservesCompletedOutcomesAndEnds(t *testing.T) {
	f := newFixture(t)
	f.workerProjects(2)
	f.executionConfig("workers: 1\n")
	ballast := filepath.Join(f.dir, "retained-ballast")
	t.Setenv("SWEEP_BALLAST", ballast)
	os.WriteFile(f.scanner, []byte(`#!/bin/sh
/usr/bin/dd if=/dev/zero of="$SWEEP_BALLAST" bs=1048576 count=16 status=none || exit 81
printf '%s\n' '{"schema_version":1,"root":"fixture","sources":[],"check_runs":[],"findings":[]}'
`), 0700)
	free := availableDiskBytes(t, f.dir)
	if free < 1024*1024*1024 {
		t.Skip("need 1 GiB free for bounded test ballast")
	}
	reserve := free - 8*1024*1024
	f.diskReserve(reserve)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "--config", f.config).CombinedOutput()
	if freeNow := availableDiskBytes(t, f.dir); freeNow >= reserve {
		t.Skip("host free space changed enough to remove the controlled shortage")
	}
	if err == nil || ctx.Err() != nil {
		t.Fatalf("persistent shortage did not end honestly: %v %s", err, out)
	}
	summary := f.summary()
	counts := summary["counts"].(map[string]any)
	if counts["complete"] != float64(1) || counts["unprocessed"] != float64(1) || counts["failed"] != float64(0) {
		t.Fatalf("outcomes=%#v", summary)
	}
	for _, raw := range summary["repositories"].([]any) {
		result := raw.(map[string]any)
		if result["outcome"] == "complete" {
			if _, err = os.Stat(filepath.Join(f.results, result["result"].(string), "scan.json")); err != nil {
				t.Fatal("completed inventory was lost")
			}
		}
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if strings.Count(string(log), "clone\n") != 1 {
		t.Fatalf("clone admitted during persistent shortage: %s", log)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestDiskReserveAcceptsExplicitZeroAndRejectsNegativeValues(t *testing.T) {
	f := newFixture(t)
	f.diskReserve(0)
	out, err := f.run()
	if err != nil {
		t.Fatalf("zero reserve rejected: %v %s", err, out)
	}
	data, _ := os.ReadFile(f.config)
	data = []byte(strings.Replace(string(data), "min_free_bytes: 0", "min_free_bytes: -1", 1))
	os.WriteFile(f.config, data, 0600)
	out, err = f.run()
	if err == nil {
		t.Fatalf("negative reserve accepted: %s", out)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestLowSpaceInOneConnectionDoesNotSkipLaterConnectionDiscovery(t *testing.T) {
	f := newFixture(t)
	later := connectionBlock("later", "gitlab", f.server.URL, "SWEEP_TEST_TOKEN", "team/project", "")
	connectionConfig(f, later)
	out, err := f.run()
	if err != nil {
		t.Fatalf("initial later-connection inventory: %v %s", err, out)
	}
	first := f.summary()["repositories"].([]any)[0].(map[string]any)["result"].(string)
	for i, path := range []string{"team/new", "team/other-new"} {
		f.projects[path] = map[string]any{"id": 43 + i, "path_with_namespace": path, "default_branch": "main", "http_url_to_repo": strings.Replace(f.server.URL, "http:", "https:", 1) + "/" + path + ".git"}
	}
	f.projects["team/empty"] = map[string]any{"id": 45, "path_with_namespace": "team/empty", "empty_repo": true}
	earlier := connectionBlock("earlier", "gitlab", f.server.URL, "SWEEP_TEST_TOKEN", "team/new", "")
	later = connectionBlock("later", "gitlab", f.server.URL, "SWEEP_TEST_TOKEN", "team/project, team/empty, team/other-new", "")
	connectionConfig(f, earlier+later)
	f.diskReserve(^uint64(0))
	os.WriteFile(filepath.Join(f.dir, "git.log"), nil, 0600)
	out, err = f.run()
	if err == nil {
		t.Fatalf("unprocessed connections accepted: %s", out)
	}
	summary := f.summary()
	counts := summary["counts"].(map[string]any)
	if counts["unprocessed"] != float64(2) || counts["reused"] != float64(1) || counts["empty"] != float64(1) || counts["failed"] != float64(0) || summary["discovery_complete"] != true {
		t.Fatalf("later connection skipped: %#v", summary)
	}
	for _, raw := range summary["repositories"].([]any) {
		row := raw.(map[string]any)
		if row["outcome"] == "reused" && (row["connection"] != "later" || row["result"] != first) {
			t.Fatalf("wrong reuse history: %#v", row)
		}
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if strings.Contains(string(log), "clone\n") {
		t.Fatalf("clone admitted below reserve: %s", log)
	}
	f.assertNoSecretOrCheckout(out)
}
