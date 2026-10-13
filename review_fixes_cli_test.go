package sweep_test

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNullScannerArraysAreNeverCompleteOrReusable(t *testing.T) {
	for _, key := range []string{"sources", "check_runs", "findings"} {
		t.Run(key, func(t *testing.T) {
			f := newFixture(t)
			output := filepath.Join(f.dir, "output.json")
			valid := map[string]any{"schema_version": 1, "root": "fixture", "sources": []any{}, "check_runs": []any{}, "findings": []any{}}
			raw, _ := json.Marshal(valid)
			os.WriteFile(output, raw, 0600)
			t.Setenv("SWEEP_SCAN_OUTPUT", output)
			os.WriteFile(f.scanner, []byte("#!/bin/sh\ncat \"$SWEEP_SCAN_OUTPUT\"\n"), 0700)
			first := reuseRun(t, f)
			invalid := map[string]any{"schema_version": 1, "root": "fixture", "sources": []any{}, "check_runs": []any{}, "findings": []any{}}
			invalid[key] = nil
			nullRaw, _ := json.Marshal(invalid)
			os.WriteFile(filepath.Join(f.results, first["result"].(string), "scan.json"), nullRaw, 0600)
			second := reuseRun(t, f)
			if second["outcome"] != "complete" || second["result"] == first["result"] {
				t.Fatalf("retained null %s was reused: %#v", key, second)
			}
			os.WriteFile(output, nullRaw, 0600)
			config, _ := os.ReadFile(f.config)
			os.WriteFile(f.config, append(config, []byte("  ignore: [changed]\n")...), 0600)
			for attempt := 0; attempt < 2; attempt++ {
				if out, err := f.run(); err == nil {
					t.Fatalf("fresh null %s accepted: %s", key, out)
				}
				row := f.summary()["repositories"].([]any)[0].(map[string]any)
				if row["outcome"] != "failed" {
					t.Fatalf("invalid scan marked complete: %#v", row)
				}
			}
		})
	}
}

func TestScannerReplacementDuringBatchCannotPublishOrReuseWrongIdentity(t *testing.T) {
	f := newFixture(t)
	f.projects["team/other"] = map[string]any{"id": 43, "path_with_namespace": "team/other", "default_branch": "main", "http_url_to_repo": strings.Replace(f.server.URL, "http:", "https:", 1) + "/team/other.git", "empty_repo": false}
	flag := filepath.Join(f.dir, "replace-scanner")
	replacement := filepath.Join(f.dir, "replacement")
	scanLog := filepath.Join(f.dir, "scanner.log")
	t.Setenv("SWEEP_REPLACE_FLAG", flag)
	t.Setenv("SWEEP_REPLACEMENT", replacement)
	t.Setenv("SWEEP_SCANNER_PATH", f.scanner)
	t.Setenv("SWEEP_SCAN_LOG", scanLog)
	oldScanner := []byte("#!/bin/sh\necho old >> \"$SWEEP_SCAN_LOG\"\nif [ -e \"$SWEEP_REPLACE_FLAG\" ]; then cp \"$SWEEP_REPLACEMENT\" \"$SWEEP_SCANNER_PATH\"; rm \"$SWEEP_REPLACE_FLAG\"; fi\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"old-version\",\"sources\":[],\"check_runs\":[],\"findings\":[]}'\n")
	os.WriteFile(f.scanner, oldScanner, 0700)
	os.WriteFile(replacement, []byte("#!/bin/sh\necho new >> \"$SWEEP_SCAN_LOG\"\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"new-version\",\"sources\":[],\"check_runs\":[],\"findings\":[]}'\n"), 0700)
	os.WriteFile(flag, nil, 0600)
	f.writeConfig("  - team/project\n  - team/other\n", "")
	config, _ := os.ReadFile(f.config)
	os.WriteFile(f.config, append([]byte("workers: 1\n"), config...), 0600)
	if out, err := f.run(); err == nil {
		t.Fatalf("scanner replacement silently succeeded: %s", out)
	}
	summary := f.summary()
	if counts := summary["counts"].(map[string]any); counts["failed"] != float64(2) || counts["complete"] != float64(0) {
		t.Fatalf("changed executable produced complete results under old identity: %#v", summary)
	}
	log, _ := os.ReadFile(scanLog)
	if string(log) != "old\n" {
		t.Fatalf("replacement scanner ran under old recorded identity: %s", log)
	}
	os.WriteFile(f.scanner, oldScanner, 0700)
	if out, err := f.run(); err != nil {
		t.Fatalf("restored scanner: %v %s", err, out)
	}
	for _, raw := range f.summary()["repositories"].([]any) {
		row := raw.(map[string]any)
		if row["outcome"] != "complete" {
			t.Fatalf("failed replacement inventory reused after restore: %#v", row)
		}
	}
}

func TestProvidersRejectNonHexadecimalCommitBeforeClone(t *testing.T) {
	for _, provider := range []string{"gitlab", "github"} {
		t.Run(provider, func(t *testing.T) {
			var f *fixture
			if provider == "github" {
				f = newGitHubFixture(t)
			} else {
				f = newFixture(t)
			}
			f.revision = strings.Repeat("g", 40)
			if out, err := f.run(); err == nil {
				t.Fatalf("invalid provider revision accepted: %s", out)
			}
			summary := f.summary()
			if summary["discovery_complete"] != false {
				t.Fatalf("provider accepted invalid revision as resolved: %#v", summary)
			}
			if log, _ := os.ReadFile(filepath.Join(f.dir, "git.log")); len(log) != 0 {
				t.Fatalf("invalid revision reached Git: %s", log)
			}
		})
	}
}

func TestScannerChangeBeforeAnalysisOrReuseIsVisible(t *testing.T) {
	for _, phase := range []string{"clone", "discovery before reuse"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			flag := filepath.Join(f.dir, "replace")
			replacement := filepath.Join(f.dir, "new-scanner")
			logPath := filepath.Join(f.dir, "scan.log")
			t.Setenv("SWEEP_SCAN_LOG", logPath)
			t.Setenv("SWEEP_REPLACE_FLAG", flag)
			t.Setenv("SWEEP_SCANNER_PATH", f.scanner)
			t.Setenv("SWEEP_REPLACEMENT", replacement)
			os.WriteFile(f.scanner, []byte("#!/bin/sh\necho old >> \"$SWEEP_SCAN_LOG\"\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"old\",\"sources\":[],\"check_runs\":[],\"findings\":[]}'\n"), 0700)
			os.WriteFile(replacement, []byte("#!/bin/sh\necho new >> \"$SWEEP_SCAN_LOG\"\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"new\",\"sources\":[],\"check_runs\":[],\"findings\":[]}'\n"), 0700)
			if phase == "clone" {
				gitPath := filepath.Join(f.dir, "bin", "git")
				raw, _ := os.ReadFile(gitPath)
				os.WriteFile(gitPath, []byte(strings.Replace(string(raw), `if [ "$1" = clone ]; then`, `if [ "$1" = clone ]; then
  if [ -e "$SWEEP_REPLACE_FLAG" ]; then cp "$SWEEP_REPLACEMENT" "$SWEEP_SCANNER_PATH"; rm "$SWEEP_REPLACE_FLAG"; fi`, 1)), 0700)
			} else {
				reuseRun(t, f)
				original := f.getHandler()
				f.setHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "/repository/commits/") {
						if _, err := os.Stat(flag); err == nil {
							raw, _ := os.ReadFile(replacement)
							os.WriteFile(f.scanner, raw, 0700)
							os.Remove(flag)
						}
					}
					original.ServeHTTP(w, r)
				}))
			}
			before, _ := os.ReadFile(logPath)
			os.WriteFile(flag, nil, 0600)
			if out, err := f.run(); err == nil || !strings.Contains(string(out), "changed during batch") {
				t.Fatalf("replacement before %s hidden: %v %s", phase, err, out)
			}
			row := f.summary()["repositories"].([]any)[0].(map[string]any)
			if row["outcome"] != "failed" || row["scanned_at"] != nil {
				t.Fatalf("changed scanner invoked or reuse claimed: %#v", row)
			}
			after, _ := os.ReadFile(logPath)
			if string(before) != string(after) {
				t.Fatalf("scanner ran with stale identity: %s", after)
			}
			if next := reuseRun(t, f); next["outcome"] != "complete" {
				t.Fatalf("new invocation did not scan replacement: %#v", next)
			}
		})
	}
}

func TestUnreapedZombieOwnerDoesNotBlockRecovery(t *testing.T) {
	f := newFixture(t)
	ready := filepath.Join(f.dir, "ready")
	t.Setenv("SWEEP_READY", ready)
	os.WriteFile(f.scanner, []byte("#!/bin/sh\nif [ ! -e \"$SWEEP_READY\" ]; then touch \"$SWEEP_READY\"; sleep 60; fi\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"fixture\",\"sources\":[],\"check_runs\":[],\"findings\":[]}'\n"), 0700)
	f.syncServer()
	cmd := exec.Command(binary, "--config", f.config)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	recoveryWaitFile(t, ready)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !recoveryProcessGone(cmd.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !recoveryProcessGone(cmd.Process.Pid) {
		t.Fatal("owner did not stop")
	}
	// Deliberately do not Wait yet: the stopped owner remains a zombie.
	if row := reuseRun(t, f); row["outcome"] != "complete" {
		t.Fatalf("unfinished zombie-owned attempt reused: %#v", row)
	}
	f.assertNoSecretOrCheckout(nil)
}

func TestPublishedInventoryRepairsLatestWithoutReplacingItWithOlderReuse(t *testing.T) {
	f := newFixture(t)
	block := filepath.Join(f.dir, "block-latest")
	t.Setenv("SWEEP_BLOCK_LATEST", block)
	os.WriteFile(f.scanner, []byte("#!/bin/sh\nif [ -e \"$SWEEP_BLOCK_LATEST\" ]; then rm \"$SWEEP_LATEST_PATH\"; mkdir \"$SWEEP_LATEST_PATH\"; fi\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"fixture\",\"sources\":[],\"check_runs\":[],\"findings\":[]}'\n"), 0700)
	first := reuseRun(t, f)
	latest := filepath.Join(f.results, filepath.Dir(filepath.Dir(first["result"].(string))), "latest.json")
	t.Setenv("SWEEP_LATEST_PATH", latest)
	oldPointer, _ := os.ReadFile(latest)
	config, _ := os.ReadFile(f.config)
	os.WriteFile(f.config, append(config, []byte("  ignore: [changed]\n")...), 0600)
	os.WriteFile(block, nil, 0600)
	if out, err := f.run(); err == nil {
		t.Fatalf("failed latest publication succeeded: %s", out)
	}
	failed := f.summary()["repositories"].([]any)[0].(map[string]any)
	if failed["outcome"] != "failed" || failed["result"] == first["result"] {
		t.Fatalf("failed publication hidden: %#v", failed)
	}
	newRef := failed["result"].(string)
	if out, err := f.run(); err == nil {
		t.Fatalf("unrepairable latest reference silently succeeded: %s", out)
	}
	os.Remove(block)
	os.Remove(latest)
	os.WriteFile(latest, oldPointer, 0600)
	newest := reuseRun(t, f)
	if newest["outcome"] != "reused" || newest["result"] != newRef {
		t.Fatalf("fully published inventory was lost: %#v", newest)
	}
	if pointer := reuseJSON(t, latest); pointer["result"] != newRef {
		t.Fatalf("latest remained stale after recovery: %#v", pointer)
	}
	os.WriteFile(f.config, config, 0600)
	older := reuseRun(t, f)
	if older["outcome"] != "reused" || older["result"] != first["result"] {
		t.Fatalf("historical matching inventory not reused: %#v", older)
	}
	if pointer := reuseJSON(t, latest); pointer["result"] != newRef {
		t.Fatalf("older reuse replaced latest complete scan: %#v", pointer)
	}
	for _, damage := range []string{"missing", "invalid"} {
		if damage == "missing" {
			os.Remove(latest)
		} else {
			os.WriteFile(latest, []byte("invalid"), 0600)
		}
		reuseRun(t, f)
		if pointer := reuseJSON(t, latest); pointer["result"] != newRef {
			t.Fatalf("%s pointer was not repaired to newest complete: %#v", damage, pointer)
		}
	}
}

func TestLatestRepairPreservesFilesBehindLinkedHistory(t *testing.T) {
	f := newFixture(t)
	first := reuseRun(t, f)
	history := filepath.Join(f.results, filepath.Dir(filepath.Dir(first["result"].(string))))
	outside := filepath.Join(f.dir, "outside-history")
	if err := os.Rename(history, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, history); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(outside, "latest.json"))
	if out, err := f.run(); err == nil {
		t.Fatalf("linked history silently accepted: %s", out)
	}
	after, err := os.ReadFile(filepath.Join(outside, "latest.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("latest repair modified files outside results history")
	}
}
