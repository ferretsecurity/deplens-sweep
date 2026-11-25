package sweep_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func reviewFixture(t *testing.T) *fixture {
	t.Helper()
	f := newGitHubFixture(t)
	f.projects["team/project"]["visibility"] = "public"
	f.projects["team/project"]["license"] = map[string]string{"spdx_id": "MIT"}
	config, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, append(config, []byte("review: true\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestReviewRejectsIneligibleRepositoriesBeforeClone(t *testing.T) {
	for _, tc := range []struct{ visibility, license string }{
		{"private", "MIT"}, {"internal", "MIT"}, {"", "MIT"}, {"public", "NOASSERTION"}, {"public", "CC-BY-NC-4.0"},
	} {
		t.Run(tc.visibility+"-"+tc.license, func(t *testing.T) {
			f := reviewFixture(t)
			f.projects["team/project"]["visibility"] = tc.visibility
			f.projects["team/project"]["license"] = map[string]string{"spdx_id": tc.license}
			out, err := f.run()
			if err == nil || !strings.Contains(string(out), "public repository with an OSI-approved license") {
				t.Fatalf("expected eligibility failure: %v %s", err, out)
			}
			if _, err := os.Stat(filepath.Join(f.dir, "git.log")); !os.IsNotExist(err) {
				t.Fatalf("ineligible repository reached git: %v", err)
			}
		})
	}
}

func installReviewCLI(t *testing.T, f *fixture, script string) {
	t.Helper()
	path := filepath.Join(f.dir, "bin", "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
}

const reviewSuccess = `
for arg do
  if [ "${last:-}" = "-o" ] || [ "${last:-}" = "--output-last-message" ]; then output=$arg; fi
  last=$arg
done
case " $* " in *" gpt-6.1-terra "*) ;; *) exit 6 ;; esac
case " $* " in *'model_reasoning_effort="medium"'*) ;; *) exit 7 ;; esac
case " $* " in *'":root"="deny"'*'network={enabled=false}'*) ;; *) exit 9 ;; esac
case " $* " in *'approval_policy="never"'*) ;; *) exit 10 ;; esac
case " $* " in *'="write"'*|*"--sandbox"*|*"--dangerously-bypass"*) exit 11 ;; esac
[ -z "${SWEEP_TEST_TOKEN:-}" ] || exit 8
report='{"inspected_inputs":true,"report":"requirements.txt:1 declares requests. The saved scan lists no sources."}'
if [ -n "${output:-}" ]; then printf '%s\n' "$report" > "$output"; else printf '%s\n' "$report"; fi
`

func TestReviewFailurePreservesInventoryForRetryAndReuse(t *testing.T) {
	f := reviewFixture(t)
	installReviewCLI(t, f, "exit 19\n")
	out, err := f.run()
	if err == nil {
		t.Fatalf("failed reviewer returned success: %s", out)
	}
	first := f.summary()["repositories"].([]any)[0].(map[string]any)
	if first["outcome"] != "complete" || first["review"].(map[string]any)["status"] != "failed" {
		t.Fatalf("inventory was lost after review failure: %#v", first)
	}
	f.assertNoSecretOrCheckout(out)

	installReviewCLI(t, f, reviewSuccess)
	second := reuseRun(t, f)
	review := second["review"].(map[string]any)
	if second["result"] != first["result"] || second["outcome"] != "reused" || review["status"] != "complete" {
		t.Fatalf("retry did not review original inventory: %#v", second)
	}
	path := filepath.Join(f.results, review["report"].(string))
	if path != filepath.Join(f.results, first["result"].(string), "review.md") {
		t.Fatalf("report path: %s", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "requirements.txt:1 declares requests") {
		t.Fatalf("report: %v %s", err, raw)
	}

	installReviewCLI(t, f, "exit 20\n")
	gitBefore, err := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if err != nil {
		t.Fatal(err)
	}
	third := reuseRun(t, f)
	gitAfter, err := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if err != nil || string(gitBefore) != string(gitAfter) || third["review"].(map[string]any)["status"] != "reused" {
		t.Fatalf("cached report required more work: %#v, %v", third, err)
	}

	if err := os.WriteFile(path, []byte("damaged report"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(); err == nil {
		t.Fatal("a changed report was reused")
	}
}

func TestReviewReportsPartialInventory(t *testing.T) {
	f := reviewFixture(t)
	if err := os.WriteFile(f.scanner, []byte(`#!/bin/sh
echo '{"schema_version":1,"root":"fixture","sources":[{"analysis":{"presence":"present","extraction":"partial"}}],"findings":[],"check_runs":[]}'
`), 0700); err != nil {
		t.Fatal(err)
	}
	installReviewCLI(t, f, reviewSuccess)
	out, err := f.run()
	if err == nil {
		t.Fatalf("partial scan returned success: %s", out)
	}
	row := f.summary()["repositories"].([]any)[0].(map[string]any)
	if row["review"].(map[string]any)["status"] != "complete" || row["outcome"] != "failed" {
		t.Fatalf("expected failed inventory with completed review: %#v", row)
	}
	path := filepath.Join(f.results, row["result"].(string))
	if _, err := os.Stat(filepath.Join(path, "scan.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, "review.md")); err != nil {
		t.Fatalf("missing report: %v", err)
	}
}

func TestReviewCannotReadInputsIsRetried(t *testing.T) {
	f := reviewFixture(t)
	inaccessible := strings.Replace(reviewSuccess, `"inspected_inputs":true`, `"inspected_inputs":false`, 1)
	installReviewCLI(t, f, inaccessible)
	out, err := f.run()
	if err == nil {
		t.Fatalf("an inaccessible checkout produced a completed review: %s", out)
	}
	first := f.summary()["repositories"].([]any)[0].(map[string]any)
	if first["outcome"] != "complete" || first["review"].(map[string]any)["status"] != "failed" {
		t.Fatalf("missing review failure or lost inventory: %#v", first)
	}
	installReviewCLI(t, f, reviewSuccess)
	second := reuseRun(t, f)
	if second["outcome"] != "reused" || second["review"].(map[string]any)["status"] != "complete" {
		t.Fatalf("original inventory was not reviewed: %#v", second)
	}
}

func TestReviewProgressGoesToStderr(t *testing.T) {
	f := reviewFixture(t)
	installReviewCLI(t, f, reviewSuccess)
	cmd := exec.Command(binary, "--config", f.config)
	var progress bytes.Buffer
	cmd.Stderr = &progress
	f.syncServer()
	out, err := cmd.Output()
	f.syncServer()
	if err != nil || !json.Valid(out) {
		t.Fatalf("expected JSON summary: %v %s", err, out)
	}
	messages := progress.String()
	for _, stage := range []string{"checking entry 1/1 (team/project)", "cloning team/project", "scanning team/project with deplens", "reviewing team/project with Codex", "[1 finished] team: team/project: complete, review complete", "Summary:"} {
		_, rest, ok := strings.Cut(messages, stage)
		if !ok {
			t.Fatalf("missing or out-of-order progress %q in %s", stage, progress.String())
		}
		messages = rest
	}
	f.assertNoSecretOrCheckout(progress.Bytes())
}
