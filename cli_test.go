package sweep_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sweep-cli-test-")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "deplens-sweep")
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/deplens-sweep")
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestMissingSelectionIsRejected(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfg, []byte("results_dir: results\nconnections:\n  - name: team\n    provider: gitlab\n    server_url: https://gitlab.example\n    token_env: SWEEP_TEST_TOKEN\n"), 0600)
	cmd := exec.Command(binary, "--config", cfg)
	cmd.Env = append(os.Environ(), "SWEEP_TEST_TOKEN=test-secret")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "explicit repository selection") {
		t.Fatalf("missing selection accepted or unclear: %v %s", err, out)
	}
}

type fixture struct {
	t                                             *testing.T
	dir, results, config, repo, revision, scanner string
	server                                        *httptest.Server
	projects                                      map[string]map[string]any
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir(), projects: map[string]map[string]any{}}
	f.results = filepath.Join(f.dir, "results")
	f.repo = filepath.Join(f.dir, "origin")
	mustRun(t, "git", "init", "--initial-branch=main", f.repo)
	os.WriteFile(filepath.Join(f.repo, "requirements.txt"), []byte("requests==2.32.3\n"), 0600)
	mustRun(t, "git", "-C", f.repo, "add", ".")
	mustRun(t, "git", "-C", f.repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "initial")
	f.revision = strings.TrimSpace(mustRun(t, "git", "-C", f.repo, "rev-parse", "HEAD"))
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "test-secret" {
			t.Errorf("missing API authentication")
			http.Error(w, "forbidden", 403)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/")
		if strings.Contains(p, "/repository/commits/") {
			json.NewEncoder(w).Encode(map[string]string{"id": f.revision})
			return
		}
		if project, ok := f.projects[p]; ok {
			json.NewEncoder(w).Encode(project)
			return
		}
		http.Error(w, "test-secret repository unavailable", 404)
	}))
	t.Cleanup(f.server.Close)
	f.projects["team/project"] = map[string]any{"id": 42, "path_with_namespace": "team/project", "default_branch": "main", "http_url_to_repo": strings.Replace(f.server.URL, "http:", "https:", 1) + "/team/project.git", "empty_repo": false}
	binDir := filepath.Join(f.dir, "bin")
	os.MkdirAll(binDir, 0700)
	// A Git boundary fixture redirects the HTTPS remote to a real local Git repository.
	// It also exercises askpass and records arguments to catch accidental credentials.
	gitScript := `#!/bin/sh
printf '%s\n' "$@" >> "$SWEEP_GIT_LOG"
if [ "$1" = clone ]; then
  [ "$GIT_LFS_SKIP_SMUDGE" = 1 ] || exit 90
  [ "$("$GIT_ASKPASS" 'Username for https://server')" = oauth2 ] || exit 91
  [ "$("$GIT_ASKPASS" 'Password for https://server')" = test-secret ] || exit 92
  for arg in "$@"; do dest="$arg"; done
  /usr/bin/git clone --no-checkout --depth 1 --single-branch --branch main "file://$SWEEP_ORIGIN" "$dest"
  exit $?
fi
if [ "$1" = -C ] && [ "$3" = fetch ]; then
 exec /usr/bin/git -C "$2" fetch --depth 1 "file://$SWEEP_ORIGIN" "$7"
fi
exec /usr/bin/git "$@"
`
	os.WriteFile(filepath.Join(binDir, "git"), []byte(gitScript), 0700)
	f.scanner = filepath.Join(binDir, "deplens")
	os.WriteFile(f.scanner, []byte("#!/bin/sh\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"fixture\",\"sources\":[],\"check_runs\":[],\"findings\":[]}'\n"), 0700)
	f.config = filepath.Join(f.dir, "config.yaml")
	f.writeConfig("  - team/project\n", "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SWEEP_TEST_TOKEN", "test-secret")
	t.Setenv("SWEEP_ORIGIN", f.repo)
	t.Setenv("SWEEP_GIT_LOG", filepath.Join(f.dir, "git.log"))
	return f
}
func (f *fixture) writeConfig(paths, extra string) {
	f.t.Helper()
	data := fmt.Sprintf("min_free_bytes: 0\nresults_dir: %s\ntemp_dir: %s\nconnections:\n  - name: team\n    provider: gitlab\n    server_url: %s\n    token_env: SWEEP_TEST_TOKEN\n    include:\n      repositories:\n%s%sdeplens:\n  executable: %s\n", f.results, filepath.Join(f.dir, "work"), f.server.URL, indent(paths, 6), extra, f.scanner)
	if err := os.WriteFile(f.config, []byte(data), 0600); err != nil {
		f.t.Fatal(err)
	}
}
func indent(s string, n int) string {
	return strings.Repeat(" ", n) + strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n"+strings.Repeat(" ", n)) + "\n"
}
func mustRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v %s", name, err, out)
	}
	return string(out)
}
func (f *fixture) run() ([]byte, error) {
	return exec.Command(binary, "--config", f.config).CombinedOutput()
}
func (f *fixture) summary() map[string]any {
	f.t.Helper()
	files, _ := filepath.Glob(filepath.Join(f.results, "batches", "*.json"))
	if len(files) == 0 {
		f.t.Fatal("no batch summary")
	}
	var value map[string]any
	data, _ := os.ReadFile(files[len(files)-1])
	if err := json.Unmarshal(data, &value); err != nil {
		f.t.Fatal(err)
	}
	return value
}
func (f *fixture) assertNoSecretOrCheckout(out []byte) {
	f.t.Helper()
	if bytes.Contains(out, []byte("test-secret")) {
		f.t.Fatal("token in command output")
	}
	filepath.Walk(f.results, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			data, _ := os.ReadFile(path)
			if bytes.Contains(data, []byte("test-secret")) {
				f.t.Errorf("token in %s", path)
			}
		}
		return err
	})
	data, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if bytes.Contains(data, []byte("test-secret")) {
		f.t.Fatal("token in Git arguments")
	}
	entries, _ := os.ReadDir(filepath.Join(f.dir, "work"))
	if len(entries) != 0 {
		f.t.Fatalf("checkouts remain: %v", entries)
	}
}
func TestExactProjectProducesInventoryAndMetadata(t *testing.T) {
	f := newFixture(t)
	out, err := f.run()
	if err != nil {
		t.Fatalf("scan: %v %s", err, out)
	}
	s := f.summary()
	results := s["repositories"].([]any)
	result := results[0].(map[string]any)
	if result["outcome"] != "complete" {
		t.Fatalf("result: %#v", result)
	}
	ref := result["result"].(string)
	var meta map[string]any
	data, e := os.ReadFile(filepath.Join(f.results, ref, "metadata.json"))
	if e != nil {
		t.Fatal(e)
	}
	json.Unmarshal(data, &meta)
	if meta["revision"] != f.revision || meta["path"] != "team/project" || meta["scanner_version"] == "" || meta["scope"] == nil {
		t.Fatalf("metadata: %s", data)
	}
	if _, e = os.Stat(filepath.Join(f.results, ref, "scan.json")); e != nil {
		t.Fatal(e)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestScanPinsResolvedCommitWhenBranchMoves(t *testing.T) {
	f := newFixture(t)
	os.WriteFile(filepath.Join(f.repo, "requirements.txt"), []byte("flask==3.1.0\n"), 0600)
	mustRun(t, "/usr/bin/git", "-C", f.repo, "add", ".")
	mustRun(t, "/usr/bin/git", "-C", f.repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "branch advanced")
	os.WriteFile(f.scanner, []byte("#!/bin/sh\nfor arg in \"$@\"; do checkout=\"$arg\"; done\ngrep -q 'requests==2.32.3' \"$checkout/requirements.txt\" || exit 8\n[ \"$(/usr/bin/git -C \"$checkout\" rev-parse --is-shallow-repository)\" = true ] || exit 9\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"fixture\",\"sources\":[],\"check_runs\":[],\"findings\":[]}'\n"), 0700)
	out, err := f.run()
	if err != nil {
		t.Fatalf("resolved revision not scanned: %v %s", err, out)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestSecretsFromExternalOutputAreNeverSaved(t *testing.T) {
	f := newFixture(t)
	os.WriteFile(f.scanner, []byte("#!/bin/sh\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"test-secret\",\"sources\":[],\"check_runs\":[],\"findings\":[],\"test-secret\":\"external secret\"}'\n"), 0700)
	out, err := f.run()
	if err != nil {
		t.Fatalf("scan: %v %s", err, out)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestAnalysisCompletenessUsesStructuredOutput(t *testing.T) {
	cases := []struct {
		name, source, runs, raw string
		failed                  bool
	}{
		{name: "unsupported and coverage gaps", source: `{"analysis":{"presence":"present","extraction":"unsupported"},"diagnostics":[{"severity":"warning","code":"coverage-gap"}]}`, runs: `[{"status":"skipped","reason_code":"coverage-gap"}]`},
		{name: "partial extraction", source: `{"analysis":{"presence":"present","extraction":"partial"}}`, runs: `[]`, failed: true},
		{name: "failed extraction", source: `{"analysis":{"presence":"unknown","extraction":"failed"}}`, runs: `[]`, failed: true},
		{name: "analysis diagnostic", source: `{"analysis":{"presence":"present","extraction":"complete"},"diagnostics":[{"severity":"error","message":"test-secret"}]}`, runs: `[]`, failed: true},
		{name: "failed check", runs: `[{"status":"failed"}]`, failed: true},
		{name: "invalid JSON", raw: `garbage test-secret`, failed: true},
		{name: "missing JSON", raw: ``, failed: true},
		{name: "unrecognized JSON", raw: `{}`, failed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			raw := tc.raw
			if tc.raw == "" && tc.name != "missing JSON" {
				source := "[]"
				if tc.source != "" {
					source = "[" + tc.source + "]"
				}
				raw = `{"schema_version":1,"root":"fixture","sources":` + source + `,"check_runs":` + tc.runs + `,"findings":[]}`
			}
			os.WriteFile(f.scanner, []byte("#!/bin/sh\ncat <<'SCAN'\n"+raw+"\nSCAN\n"), 0700)
			out, err := f.run()
			if (err != nil) != tc.failed {
				t.Fatalf("failed=%v err=%v output=%s", tc.failed, err, out)
			}
			result := f.summary()["repositories"].([]any)[0].(map[string]any)
			expected := "complete"
			if tc.failed {
				expected = "failed"
			}
			if result["outcome"] != expected {
				t.Fatalf("result=%#v", result)
			}
			ref := result["result"].(string)
			_, scanErr := os.Stat(filepath.Join(f.results, ref, "scan.json"))
			if json.Valid([]byte(raw)) && scanErr != nil {
				t.Fatal("valid partial scan was not retained")
			}
			if !json.Valid([]byte(raw)) && scanErr == nil {
				t.Fatal("invalid scan was published")
			}
			latest, _ := filepath.Glob(filepath.Join(f.results, "connections", "team", "*", "latest.json"))
			if tc.failed && len(latest) != 0 {
				t.Fatal("failed attempt published as latest complete")
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestIndividualFailuresDoNotStopOtherSelectedRepositories(t *testing.T) {
	f := newFixture(t)
	f.projects["team/empty"] = map[string]any{"id": 43, "path_with_namespace": "team/empty", "empty_repo": true}
	f.writeConfig("  - team/inaccessible\n  - team/empty\n  - team/project\n", "")
	out, err := f.run()
	if err == nil {
		t.Fatal("inaccessible repository accepted")
	}
	s := f.summary()
	counts := s["counts"].(map[string]any)
	if counts["failed"] != float64(1) || counts["empty"] != float64(1) || counts["complete"] != float64(1) || s["discovery_complete"] != false {
		t.Fatalf("summary=%#v", s)
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if strings.Count(string(log), "clone\n") != 1 {
		t.Fatalf("empty or inaccessible repository cloned: %s", log)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestExclusionsDuplicatesArchivedAndForkSelections(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			f := newFixture(t)
			f.projects["team/excluded"] = map[string]any{"id": 43, "path_with_namespace": "team/excluded", "empty_repo": true}
			f.projects["team/archive"] = map[string]any{"id": 44, "path_with_namespace": "team/archive", "empty_repo": true, "archived": true}
			f.projects["team/fork"] = map[string]any{"id": 45, "path_with_namespace": "team/fork", "empty_repo": true, "forked_from_project": map[string]any{"id": 99}}
			extra := "    exclude:\n      repositories: [team/excluded]\n"
			if enabled {
				extra += "    include_archived: true\n    include_forks: true\n"
			}
			f.writeConfig("  - team/excluded\n  - team/project\n  - team/archive\n  - team/fork\n  - team/project\n  - team/excluded\n", extra)
			out, err := f.run()
			if err != nil {
				t.Fatalf("%v %s", err, out)
			}
			s := f.summary()
			rows := s["repositories"].([]any)
			expected := 1
			if enabled {
				expected = 3
			}
			if len(rows) != expected {
				t.Fatalf("results=%#v", rows)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestRealDeplensProducesDependencyInventory(t *testing.T) {
	scanner := os.Getenv("SWEEP_REAL_DEPLENS")
	if scanner == "" {
		t.Skip("set SWEEP_REAL_DEPLENS to an installed real deplens executable")
	}
	f := newFixture(t)
	f.scanner = scanner
	f.writeConfig("  - team/project\n", "")
	out, err := f.run()
	if err != nil {
		t.Fatalf("real scan: %v %s", err, out)
	}
	result := f.summary()["repositories"].([]any)[0].(map[string]any)
	data, err := os.ReadFile(filepath.Join(f.results, result["result"].(string), "scan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var scan struct {
		Sources []struct {
			Path         string `json:"path"`
			Dependencies []struct {
				Name    string `json:"name"`
				Version string `json:"version_constraint"`
			} `json:"dependencies"`
		} `json:"sources"`
	}
	if err = json.Unmarshal(data, &scan); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, source := range scan.Sources {
		for _, dep := range source.Dependencies {
			if source.Path == "requirements.txt" && dep.Name == "requests" && dep.Version == "==2.32.3" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected requirements inventory: %s", data)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestMalformedSourceAnalysisCannotBecomeComplete(t *testing.T) {
	f := newFixture(t)
	os.WriteFile(f.scanner, []byte("#!/bin/sh\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"fixture\",\"sources\":[{\"analysis\":{\"extraction\":\"complete\"}}],\"check_runs\":[],\"findings\":[]}'\n"), 0700)
	out, err := f.run()
	if err == nil {
		t.Fatalf("malformed analysis accepted: %s", out)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestEarlierAttemptsSurviveFailedCurrentScan(t *testing.T) {
	f := newFixture(t)
	out, err := f.run()
	if err != nil {
		t.Fatalf("first scan: %v %s", err, out)
	}
	first := f.summary()["repositories"].([]any)[0].(map[string]any)["result"].(string)
	os.WriteFile(f.scanner, []byte("#!/bin/sh\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"test-secret\",\"sources\":[],\"check_runs\":[],\"findings\":[]}'\necho 'test-secret failed' >&2\nexit 7\n"), 0700)
	out, err = f.run()
	if err == nil {
		t.Fatalf("process failure accepted: %s", out)
	}
	current := f.summary()["repositories"].([]any)[0].(map[string]any)
	if current["outcome"] != "failed" || current["result"] == first {
		t.Fatalf("previous result substituted: %#v", current)
	}
	if _, err = os.Stat(filepath.Join(f.results, first, "scan.json")); err != nil {
		t.Fatal("previous inventory removed")
	}
	latestFiles, _ := filepath.Glob(filepath.Join(f.results, "connections", "team", "*", "latest.json"))
	var latest map[string]any
	data, _ := os.ReadFile(latestFiles[0])
	json.Unmarshal(data, &latest)
	if latest["result"] != first {
		t.Fatalf("failed attempt replaced latest complete: %s", data)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestCloneFailureCleansUpAndContinues(t *testing.T) {
	f := newFixture(t)
	f.projects["team/failing"] = map[string]any{"id": 43, "path_with_namespace": "team/failing", "default_branch": "main", "http_url_to_repo": strings.Replace(f.server.URL, "http:", "https:", 1) + "/team/failing.git"}
	f.writeConfig("  - team/failing\n  - team/project\n", "")
	gitPath := filepath.Join(f.dir, "bin", "git")
	data, _ := os.ReadFile(gitPath)
	script := strings.Replace(string(data), "if [ \"$1\" = clone ]; then", `if [ "$1" = clone ]; then
  case "$*" in
    *failing.git*) for arg in "$@"; do dest="$arg"; done; mkdir -p "$dest"; echo 'test-secret clone failure' >&2; exit 3 ;;
  esac`, 1)
	os.WriteFile(gitPath, []byte(script), 0700)
	out, err := f.run()
	if err == nil {
		t.Fatal("clone failure accepted")
	}
	counts := f.summary()["counts"].(map[string]any)
	if counts["failed"] != float64(1) || counts["complete"] != float64(1) {
		t.Fatalf("outcomes=%#v", counts)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestPermissionErrorIsFailureAndNotEmpty(t *testing.T) {
	f := newFixture(t)
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "test-secret access denied", 403) })
	out, err := f.run()
	if err == nil {
		t.Fatal("permission error accepted")
	}
	counts := f.summary()["counts"].(map[string]any)
	if counts["failed"] != float64(1) || counts["empty"] != float64(0) {
		t.Fatalf("outcomes=%#v", counts)
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if len(log) != 0 {
		t.Fatal("inaccessible repository cloned")
	}
	f.assertNoSecretOrCheckout(out)
}

func TestUnsupportedConfigurationIsRejected(t *testing.T) {
	for _, extra := range []string{"workers: 0\n", "connections: []\n", "timeout: 0s\n", "all_accessible: true\n"} {
		t.Run(strings.TrimSpace(extra), func(t *testing.T) {
			f := newFixture(t)
			data, _ := os.ReadFile(f.config)
			os.WriteFile(f.config, append(data, []byte(extra)...), 0600)
			out, err := f.run()
			if err == nil {
				t.Fatalf("unsupported configuration accepted: %s", out)
			}
		})
	}
}

func TestArchiveAndForkOptionsAreIndependent(t *testing.T) {
	for _, option := range []string{"include_archived", "include_forks"} {
		t.Run(option, func(t *testing.T) {
			f := newFixture(t)
			f.projects["team/archive"] = map[string]any{"id": 43, "path_with_namespace": "team/archive", "empty_repo": true, "archived": true}
			f.projects["team/fork"] = map[string]any{"id": 44, "path_with_namespace": "team/fork", "empty_repo": true, "forked_from_project": map[string]any{"id": 99}}
			f.writeConfig("  - team/archive\n  - team/fork\n", "    "+option+": true\n")
			out, err := f.run()
			if err != nil {
				t.Fatalf("%v %s", err, out)
			}
			rows := f.summary()["repositories"].([]any)
			expected := "team/archive"
			if option == "include_forks" {
				expected = "team/fork"
			}
			if len(rows) != 1 || rows[0].(map[string]any)["path"] != expected {
				t.Fatalf("filter options coupled: %#v", rows)
			}
		})
	}
}

func TestCredentialBearingCloneURLIsRejectedWithoutLeak(t *testing.T) {
	f := newFixture(t)
	f.projects["team/project"]["http_url_to_repo"] = "https://oauth2:test-secret@" + strings.TrimPrefix(f.server.URL, "http://") + "/team/project.git"
	out, err := f.run()
	if err == nil {
		t.Fatal("credential-bearing clone URL accepted")
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if len(log) != 0 {
		t.Fatal("credential-bearing URL passed to Git")
	}
	f.assertNoSecretOrCheckout(out)
}
