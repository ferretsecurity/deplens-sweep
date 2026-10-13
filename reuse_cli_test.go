package sweep_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reuseRun(t *testing.T, f *fixture) map[string]any {
	t.Helper()
	if out, err := f.run(); err != nil {
		t.Fatalf("batch: %v %s", err, out)
	}
	return f.summary()["repositories"].([]any)[0].(map[string]any)
}

func TestReuseInvalidationUsesCurrentInputsAndRetainsHistory(t *testing.T) {
	for _, change := range []string{"commit", "scanner", "ignore", "disable_rule", "disable_check", "exclude_preset", "rules", "extend_rules"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			rules := filepath.Join(f.dir, "rules.yaml")
			os.WriteFile(rules, []byte("rules: []\n"), 0600)
			if change == "rules" || change == "extend_rules" {
				config, _ := os.ReadFile(f.config)
				if change == "rules" {
					config = append(config, []byte("  rules: "+rules+"\n")...)
				} else {
					config = append(config, []byte("  extend_rules: ["+rules+"]\n")...)
				}
				os.WriteFile(f.config, config, 0600)
			}
			first := reuseRun(t, f)
			switch change {
			case "commit":
				os.WriteFile(filepath.Join(f.repo, "requirements.txt"), []byte("flask==3.1.0\n"), 0600)
				mustRun(t, "/usr/bin/git", "-C", f.repo, "add", ".")
				mustRun(t, "/usr/bin/git", "-C", f.repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "advance")
				f.revision = strings.TrimSpace(mustRun(t, "/usr/bin/git", "-C", f.repo, "rev-parse", "HEAD"))
			case "scanner":
				raw, _ := os.ReadFile(f.scanner)
				os.WriteFile(f.scanner, append(raw, []byte("# new version\n")...), 0700)
			case "rules", "extend_rules":
				os.WriteFile(rules, []byte("rules: []\n# edited in place\n"), 0600)
			default:
				raw, _ := os.ReadFile(f.config)
				key := map[string]string{"ignore": "ignore", "disable_rule": "disable_rules", "disable_check": "disable_checks", "exclude_preset": "exclude_presets"}[change]
				os.WriteFile(f.config, append(raw, []byte("  "+key+": [changed]\n")...), 0600)
			}
			second := reuseRun(t, f)
			if second["outcome"] != "complete" || second["result"] == first["result"] {
				t.Fatalf("changed %s reused: %#v", change, second)
			}
			if _, err := os.Stat(filepath.Join(f.results, first["result"].(string), "scan.json")); err != nil {
				t.Fatalf("lost history: %v", err)
			}
			third := reuseRun(t, f)
			if third["outcome"] != "reused" || third["result"] != second["result"] {
				t.Fatalf("new inputs not reusable: %#v", third)
			}
		})
	}
}

func TestReuseSurvivesRenameDeselectionAndReturnsToHistoricalInputs(t *testing.T) {
	f := newFixture(t)
	first := reuseRun(t, f)
	project := f.projects["team/project"]
	delete(f.projects, "team/project")
	project["path_with_namespace"] = "team/renamed"
	f.projects["team/renamed"] = project
	f.writeConfig("  - team/renamed\n", "")
	renamed := reuseRun(t, f)
	if renamed["outcome"] != "reused" || renamed["path"] != "team/renamed" || renamed["result"] != first["result"] {
		t.Fatalf("rename lost history: %#v", renamed)
	}
	f.writeConfig("  - team/renamed\n", "    exclude:\n      repositories: [team/renamed]\n")
	if out, err := f.run(); err != nil {
		t.Fatalf("deselection: %v %s", err, out)
	}
	if rows := f.summary()["repositories"].([]any); len(rows) != 0 {
		t.Fatalf("historical inventory appeared in current batch: %#v", rows)
	}
	f.writeConfig("  - team/renamed\n", "")
	if row := reuseRun(t, f); row["result"] != first["result"] || row["outcome"] != "reused" {
		t.Fatalf("reselection lost history: %#v", row)
	}
	raw, _ := os.ReadFile(f.config)
	os.WriteFile(f.config, append(raw, []byte("  disable_rules: [temporary]\n")...), 0600)
	newer := reuseRun(t, f)
	os.WriteFile(f.config, raw, 0600)
	old := reuseRun(t, f)
	if old["result"] != first["result"] || old["outcome"] != "reused" {
		t.Fatalf("matching older inventory was ignored: %#v", old)
	}
	latest := reuseJSON(t, filepath.Join(f.results, filepath.Dir(filepath.Dir(newer["result"].(string))), "latest.json"))
	if latest["result"] != newer["result"] {
		t.Fatalf("reuse rewrote latest actual complete scan: %#v", latest)
	}
}

func TestUnchangedCompleteInventoryIsReusedWithoutCloneOrScanner(t *testing.T) {
	f := newFixture(t)
	scannerScript, _ := os.ReadFile(f.scanner)
	scannerScript = append([]byte("#!/bin/sh\necho scan >> \"$SWEEP_SCAN_LOG\"\n"), scannerScript...)
	os.WriteFile(f.scanner, scannerScript, 0700)
	t.Setenv("SWEEP_SCAN_LOG", filepath.Join(f.dir, "scan.log"))
	first := reuseRun(t, f)
	gitBefore, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	scanBefore, _ := os.ReadFile(filepath.Join(f.dir, "scan.log"))
	second := reuseRun(t, f)
	gitAfter, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	scanAfter, _ := os.ReadFile(filepath.Join(f.dir, "scan.log"))
	if second["outcome"] != "reused" || second["result"] != first["result"] || string(gitBefore) != string(gitAfter) || string(scanBefore) != string(scanAfter) {
		t.Fatalf("unchanged scan performed work or lost result reference: %#v", second)
	}
	if second["scanned_at"] != first["scanned_at"] || second["scanned_at"] == nil {
		t.Fatalf("scan time changed or absent: %#v", second)
	}
	checked, _ := time.Parse(time.RFC3339Nano, second["checked_at"].(string))
	scanned, _ := time.Parse(time.RFC3339Nano, second["scanned_at"].(string))
	if !checked.After(scanned) {
		t.Fatalf("check time does not distinguish reuse: %#v", second)
	}
	metadata := reuseJSON(t, filepath.Join(f.results, first["result"].(string), "metadata.json"))
	if metadata["input_fingerprint"] == nil {
		t.Fatal("saved inputs have no fingerprint")
	}
	if metadata["scanned_at"] != first["scanned_at"] {
		t.Fatal("scan time is not preserved in original metadata")
	}
	files, _ := filepath.Glob(filepath.Join(f.results, "connections", "team", "*", "attempts", "*", "scan.json"))
	if len(files) != 1 {
		t.Fatalf("reuse copied inventory: %v", files)
	}
}

func reuseJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestReuseRejectsMissingDamagedAndIncompleteArtifacts(t *testing.T) {
	for _, damage := range []string{"missing scan", "missing metadata", "invalid scan", "partial scan", "invalid metadata", "foreign identity", "linked scan", "linked attempt"} {
		t.Run(damage, func(t *testing.T) {
			f := newFixture(t)
			first := reuseRun(t, f)
			attempt := filepath.Join(f.results, first["result"].(string))
			scan := filepath.Join(attempt, "scan.json")
			metadata := filepath.Join(attempt, "metadata.json")
			switch damage {
			case "missing scan":
				os.Remove(scan)
			case "missing metadata":
				os.Remove(metadata)
			case "invalid scan":
				os.WriteFile(scan, []byte(`{"schema_version":1}`), 0600)
			case "partial scan":
				os.WriteFile(scan, []byte(`{"schema_version":1,"root":"fixture","sources":[{"analysis":{"presence":"present","extraction":"partial"}}],"check_runs":[],"findings":[]}`), 0600)
			case "invalid metadata":
				os.WriteFile(metadata, []byte(`{"outcome":"complete"}`), 0600)
			case "foreign identity":
				meta := reuseJSON(t, metadata)
				meta["repository_id"] = "99"
				raw, _ := json.Marshal(meta)
				os.WriteFile(metadata, raw, 0600)
			case "linked scan":
				outside := filepath.Join(f.dir, "outside-scan.json")
				if err := os.Rename(scan, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, scan); err != nil {
					t.Fatal(err)
				}
			case "linked attempt":
				outside := filepath.Join(f.dir, "outside-attempt")
				if err := os.Rename(attempt, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, attempt); err != nil {
					t.Fatal(err)
				}
			}
			second := reuseRun(t, f)
			if second["outcome"] != "complete" || second["result"] == first["result"] {
				t.Fatalf("damaged artifacts reused: %#v", second)
			}
		})
	}
}

func TestIncompleteResultsRetryAndKeepOlderCompleteResultVisible(t *testing.T) {
	f := newFixture(t)
	output := filepath.Join(f.dir, "scanner-output.json")
	complete := `{"schema_version":1,"root":"fixture","sources":[],"check_runs":[],"findings":[]}`
	os.WriteFile(output, []byte(complete), 0600)
	os.WriteFile(f.scanner, []byte("#!/bin/sh\ncat \"$SWEEP_SCAN_OUTPUT\"\n"), 0700)
	t.Setenv("SWEEP_SCAN_OUTPUT", output)
	first := reuseRun(t, f)
	latestPath := filepath.Join(f.results, filepath.Dir(filepath.Dir(first["result"].(string))), "latest.json")
	raw, _ := os.ReadFile(f.config)
	os.WriteFile(f.config, append(raw, []byte("  ignore: [different]\n")...), 0600)
	for _, state := range []string{"partial", "failed"} {
		os.WriteFile(output, []byte(`{"schema_version":1,"root":"fixture","sources":[{"analysis":{"presence":"present","extraction":"`+state+`"}}],"check_runs":[],"findings":[]}`), 0600)
		previousRef := ""
		for retry := 0; retry < 2; retry++ {
			if out, err := f.run(); err == nil {
				t.Fatalf("incomplete batch succeeded: %s", out)
			}
			row := f.summary()["repositories"].([]any)[0].(map[string]any)
			if row["outcome"] != "failed" || row["result"] == first["result"] || row["result"] == previousRef {
				t.Fatalf("incomplete attempt reused or hidden: %#v", row)
			}
			previousRef = row["result"].(string)
			if latest := reuseJSON(t, latestPath); latest["result"] != first["result"] {
				t.Fatalf("failure replaced older complete result: %#v", latest)
			}
		}
	}
	os.WriteFile(output, []byte(complete), 0600)
	recovered := reuseRun(t, f)
	if recovered["outcome"] != "complete" || recovered["result"] == first["result"] {
		t.Fatalf("retry failed to produce inventory: %#v", recovered)
	}
	if reused := reuseRun(t, f); reused["outcome"] != "reused" || reused["result"] != recovered["result"] {
		t.Fatalf("successful retry not reusable: %#v", reused)
	}
}

func TestUnsupportedExtractionAndCoverageGapsRemainReusable(t *testing.T) {
	f := newFixture(t)
	os.WriteFile(f.scanner, []byte("#!/bin/sh\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"fixture\",\"sources\":[{\"analysis\":{\"presence\":\"present\",\"extraction\":\"unsupported\"},\"diagnostics\":[{\"severity\":\"warning\",\"code\":\"coverage-gap\"}]}],\"check_runs\":[{\"status\":\"skipped\",\"reason_code\":\"coverage-gap\"}],\"findings\":[]}'\n"), 0700)
	first := reuseRun(t, f)
	second := reuseRun(t, f)
	if second["outcome"] != "reused" || second["result"] != first["result"] {
		t.Fatalf("unsupported result retried: %#v", second)
	}
}

func TestRepositoryDisappearanceKeepsHistoryForLaterSelection(t *testing.T) {
	f := newFixture(t)
	first := reuseRun(t, f)
	project := f.projects["team/project"]
	delete(f.projects, "team/project")
	if out, err := f.run(); err == nil {
		t.Fatalf("disappeared repository succeeded: %s", out)
	}
	summary := f.summary()
	if summary["discovery_complete"] != false || summary["repositories"].([]any)[0].(map[string]any)["outcome"] != "failed" {
		t.Fatalf("missing current repository hidden: %#v", summary)
	}
	if _, err := os.Stat(filepath.Join(f.results, first["result"].(string), "scan.json")); err != nil {
		t.Fatalf("disappearance removed history: %v", err)
	}
	f.projects["team/project"] = project
	if selected := reuseRun(t, f); selected["outcome"] != "reused" || selected["result"] != first["result"] {
		t.Fatalf("returning repository lost inventory: %#v", selected)
	}
}

func TestMovingIdenticalRulesPreservesEffectiveInputs(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.dir, "rules.yaml")
	os.WriteFile(path, []byte("rules: []\n"), 0600)
	raw, _ := os.ReadFile(f.config)
	os.WriteFile(f.config, append(raw, []byte("  rules: "+path+"\n")...), 0600)
	first := reuseRun(t, f)
	moved := filepath.Join(f.dir, "renamed-rules.yaml")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(f.config, append(raw, []byte("  rules: "+moved+"\n")...), 0600)
	second := reuseRun(t, f)
	if second["outcome"] != "reused" || second["result"] != first["result"] {
		t.Fatalf("rule locator invalidated identical inputs: %#v", second)
	}
}

func TestRealDeplensInventoryCanBeReused(t *testing.T) {
	scanner := os.Getenv("SWEEP_REAL_DEPLENS")
	if scanner == "" {
		t.Skip("set SWEEP_REAL_DEPLENS to an installed real deplens executable")
	}
	f := newFixture(t)
	f.scanner = scanner
	f.writeConfig("  - team/project\n", "")
	first := reuseRun(t, f)
	second := reuseRun(t, f)
	if second["outcome"] != "reused" || second["result"] != first["result"] {
		t.Fatalf("real inventory not reusable: %#v", second)
	}
}
