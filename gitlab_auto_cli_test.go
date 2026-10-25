package sweep_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestGitLabAutoRecoversRepeated500(t *testing.T) {
	for _, mode := range []string{"omitted", "", "auto"} {
		for _, laterPage := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/later=%v", mode, laterPage), func(t *testing.T) {
				f := newFixture(t)
				f.projects["team/sub/nested"] = gitlabProjectFixture(f, 43, "team/sub/nested")
				failedCalls, walks := 0, 0
				gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v4/groups/team/projects" && r.URL.Query().Get("include_subgroups") == "true" {
						if laterPage && r.URL.Query().Get("page") == "" {
							w.Header().Set("X-Next-Page", "2")
							json.NewEncoder(w).Encode([]any{f.projects["team/project"]})
							return
						}
						failedCalls++
						http.Error(w, "test-secret provider body", 500)
						return
					}
					walks++
					switch r.URL.Path {
					case "/api/v4/groups/team/projects":
						json.NewEncoder(w).Encode([]any{f.projects["team/project"]})
					case "/api/v4/groups/team/subgroups":
						fmt.Fprint(w, `[{"id":2,"full_path":"team/sub"}]`)
					case "/api/v4/groups/team/sub/projects":
						json.NewEncoder(w).Encode([]any{f.projects["team/sub/nested"]})
					case "/api/v4/groups/team/sub/subgroups":
						fmt.Fprint(w, "[]")
					default:
						t.Errorf("unexpected request: %s", r.URL)
						http.Error(w, "unexpected", 400)
					}
				})
				extra := "      groups: [team, team/sub, team]\n"
				if mode != "omitted" {
					extra += "    gitlab_group_discovery: '" + mode + "'\n"
				}
				f.writeConfig("  - team/project\n", extra)
				out, err := f.run()
				if err != nil || failedCalls != 2 || walks != 3 {
					t.Fatalf("recursive=%d walk=%d err=%v %s", failedCalls, walks, err, out)
				}
				s := f.summary()
				if s["discovery_complete"] != true || s["counts"].(map[string]any)["complete"] != float64(2) || len(s["repositories"].([]any)) != 2 {
					t.Fatalf("summary: %#v", s)
				}
				if !strings.Contains(string(out), `connection "team" group "team": switching to explicit subgroup walking`) {
					t.Fatalf("missing diagnostic: %s", out)
				}
				f.assertNoSecretOrCheckout(out)
			})
		}
	}
}

func TestGitLabAutoFallbackIsLocalToGroup(t *testing.T) {
	f := newFixture(t)
	calls := map[string]int{}
	gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		calls[r.URL.RequestURI()]++
		if r.URL.Path == "/api/v4/groups/team/projects" && r.URL.Query().Get("include_subgroups") == "true" {
			http.Error(w, "test-secret", 500)
			return
		}
		fmt.Fprint(w, "[]")
	})
	f.writeConfig("", "      groups: [team, other]\n")
	data, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	other := fmt.Sprintf("  - name: independent\n    provider: gitlab\n    server_url: %s\n    token_env: SWEEP_TEST_TOKEN\n    include:\n      groups: [other]\n", f.server.URL)
	if err := os.WriteFile(f.config, []byte(strings.Replace(string(data), "deplens:\n", other+"deplens:\n", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	for batch := 1; batch <= 2; batch++ {
		out, err := f.run()
		if err != nil {
			t.Fatalf("batch %d: %v %s", batch, err, out)
		}
		for query, want := range map[string]int{
			"/api/v4/groups/team/projects?include_subgroups=true&with_shared=false&per_page=100":   2 * batch,
			"/api/v4/groups/team/projects?include_subgroups=false&with_shared=false&per_page=100":  batch,
			"/api/v4/groups/other/projects?include_subgroups=false&with_shared=false&per_page=100": 0,
			"/api/v4/groups/other/projects?include_subgroups=true&with_shared=false&per_page=100":  2 * batch,
			"/api/v4/groups/other/subgroups?per_page=100":                                          0,
		} {
			if calls[query] != want {
				t.Errorf("batch %d %s: %d want %d", batch, query, calls[query], want)
			}
		}
		if strings.Count(string(out), "switching to explicit subgroup walking") != 1 || f.summary()["discovery_complete"] != true {
			t.Fatalf("batch %d: %s", batch, out)
		}
		f.assertNoSecretOrCheckout(out)
	}
}

func TestGitLabAutoNestedFallbackKeepsHealthySubtreesRecursive(t *testing.T) {
	f := newFixture(t)
	for i, path := range []string{"team/broken/one", "team/broken/deep/two", "team/healthy/three", "team/healthy/nested/four", "team/secret/hidden", "team/archived", "team/fork"} {
		f.projects[path] = gitlabProjectFixture(f, 50+i, path)
	}
	f.projects["team/archived"]["archived"] = true
	f.projects["team/fork"]["forked_from_project"] = map[string]any{"id": 100}
	calls := map[string]int{}
	gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		calls[r.URL.RequestURI()]++
		recursive := r.URL.Query().Get("include_subgroups") == "true"
		if strings.HasSuffix(r.URL.Path, "/projects") && (r.URL.Query().Get("with_shared") != "false" || r.URL.Query().Get("per_page") != "100") {
			t.Errorf("incorrect project query: %s", r.URL)
		}
		switch r.URL.Path {
		case "/api/v4/groups/team/projects":
			if recursive {
				http.Error(w, "test-secret", 500)
				return
			}
			json.NewEncoder(w).Encode([]any{f.projects["team/project"]})
		case "/api/v4/groups/team/subgroups":
			fmt.Fprint(w, `[{"id":2,"full_path":"team/broken"},{"id":3,"full_path":"team/healthy"},{"id":4,"full_path":"team/empty"},{"id":5,"full_path":"team/secret"}]`)
		case "/api/v4/groups/team/broken/projects":
			if recursive {
				if r.URL.Query().Get("page") == "2" {
					http.Error(w, "test-secret", 500)
					return
				}
				w.Header().Set("X-Next-Page", "2")
			}
			json.NewEncoder(w).Encode([]any{f.projects["team/broken/one"]})
		case "/api/v4/groups/team/broken/subgroups":
			fmt.Fprint(w, `[{"id":6,"full_path":"team/broken/deep"}]`)
		case "/api/v4/groups/team/broken/deep/projects":
			if !recursive {
				t.Error("healthy grandchild must use recursive listing")
			}
			json.NewEncoder(w).Encode([]any{f.projects["team/broken/deep/two"]})
		case "/api/v4/groups/team/healthy/projects":
			if !recursive {
				t.Error("healthy sibling must use recursive listing")
			}
			if r.URL.Query().Get("page") == "2" {
				json.NewEncoder(w).Encode([]any{f.projects["team/healthy/nested/four"]})
				return
			}
			w.Header().Set("X-Next-Page", "2")
			json.NewEncoder(w).Encode([]any{f.projects["team/healthy/three"], f.projects["team/secret/hidden"], f.projects["team/archived"], f.projects["team/fork"]})
		case "/api/v4/groups/team/empty/projects":
			if !recursive {
				t.Error("empty group must use recursive listing")
			}
			fmt.Fprint(w, "[]")
		default:
			t.Errorf("unnecessary traversal: %s", r.URL)
			http.Error(w, "unexpected", 400)
		}
	})
	f.writeConfig("", "      groups: [team, team/broken, team/healthy, team]\n    exclude:\n      groups: [team/secret]\n")
	out, err := f.run()
	if err != nil {
		t.Fatalf("nested fallback: %v %s", err, out)
	}
	for query, count := range calls {
		want := 1
		if query == "/api/v4/groups/team/projects?include_subgroups=true&with_shared=false&per_page=100" || query == "/api/v4/groups/team%2Fbroken/projects?include_subgroups=true&page=2&per_page=100&with_shared=false" {
			want = 2
		}
		if count != want {
			t.Errorf("%s: got %d requests, want %d", query, count, want)
		}
	}
	s := f.summary()
	if s["discovery_complete"] != true || s["counts"].(map[string]any)["complete"] != float64(5) || len(s["repositories"].([]any)) != 5 {
		t.Fatalf("summary: %#v", s)
	}
	if strings.Count(string(out), "switching to explicit subgroup walking") != 2 {
		t.Fatalf("expected root and nested fallback diagnostics: %s", out)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitLabAutoChildFailureDoesNotWalkOrStopSibling(t *testing.T) {
	f := newFixture(t)
	f.projects["team/healthy/project"] = gitlabProjectFixture(f, 43, "team/healthy/project")
	gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/groups/team/projects":
			if r.URL.Query().Get("include_subgroups") == "true" {
				http.Error(w, "test-secret", 500)
				return
			}
			fmt.Fprint(w, "[]")
		case "/api/v4/groups/team/subgroups":
			fmt.Fprint(w, `[{"id":2,"full_path":"team/bad"},{"id":3,"full_path":"team/healthy"}]`)
		case "/api/v4/groups/team/bad/projects":
			if r.URL.Query().Get("include_subgroups") != "true" {
				t.Error("permission failure must not trigger walking")
			}
			http.Error(w, "test-secret", 403)
		case "/api/v4/groups/team/healthy/projects":
			if r.URL.Query().Get("include_subgroups") != "true" {
				t.Error("healthy sibling must use recursive listing")
			}
			json.NewEncoder(w).Encode([]any{f.projects["team/healthy/project"]})
		default:
			t.Errorf("unexpected traversal: %s", r.URL)
			http.Error(w, "unexpected", 400)
		}
	})
	f.writeConfig("", "      groups: [team]\n")
	out, err := f.run()
	s := f.summary()
	counts := s["counts"].(map[string]any)
	if err == nil || s["discovery_complete"] != false || counts["failed"] != float64(1) || counts["complete"] != float64(1) {
		t.Fatalf("err=%v summary=%#v", err, s)
	}
	if strings.Count(string(out), "switching to explicit subgroup walking") != 1 {
		t.Fatalf("expected only root fallback: %s", out)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitLabAutoPreservesUnrecoveredFailures(t *testing.T) {
	for _, failure := range []string{"walk-projects", "walk-subgroups", "repository", "exact-selection", "acquisition"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			f.projects["team/sub/nested"] = gitlabProjectFixture(f, 43, "team/sub/nested")
			if failure == "repository" {
				f.projects["team/project"]["default_branch"] = ""
			}
			recursive, direct := 0, 0
			gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("include_subgroups") == "true" {
					recursive++
					if r.URL.Path == "/api/v4/groups/team/sub/projects" {
						json.NewEncoder(w).Encode([]any{f.projects["team/sub/nested"]})
						return
					}
					if r.URL.Query().Get("page") == "" {
						w.Header().Set("X-Next-Page", "2")
						json.NewEncoder(w).Encode([]any{f.projects["team/project"]})
					} else {
						http.Error(w, "test-secret", 500)
					}
					return
				}
				switch r.URL.Path {
				case "/api/v4/groups/team/projects":
					direct++
					if failure == "walk-projects" {
						http.Error(w, "test-secret", 500)
						return
					}
					json.NewEncoder(w).Encode([]any{f.projects["team/project"]})
				case "/api/v4/groups/team/subgroups":
					if failure == "walk-subgroups" {
						if r.URL.Query().Get("page") == "2" {
							http.Error(w, "test-secret", 403)
							return
						}
						w.Header().Set("X-Next-Page", "2")
					}
					fmt.Fprint(w, `[{"id":2,"full_path":"team/sub"}]`)
				case "/api/v4/groups/team/sub/projects":
					json.NewEncoder(w).Encode([]any{f.projects["team/sub/nested"]})
				case "/api/v4/groups/team/sub/subgroups":
					fmt.Fprint(w, "[]")
				case "/api/v4/projects/missing/project":
					http.Error(w, "test-secret", 404)
				default:
					t.Errorf("unexpected: %s", r.URL)
					http.Error(w, "unexpected", 400)
				}
			})
			paths := ""
			if failure == "exact-selection" {
				paths = "  - missing/project\n"
			}
			if failure == "acquisition" {
				gitPath := filepath.Join(f.dir, "bin", "git")
				data, err := os.ReadFile(gitPath)
				if err != nil {
					t.Fatal(err)
				}
				script := strings.Replace(string(data), `if [ "$1" = clone ]; then`, `if [ "$1" = clone ]; then
  case "$*" in
    */team/project.git*) echo 'test-secret clone failure' >&2; exit 3 ;;
  esac`, 1)
				if err := os.WriteFile(gitPath, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			f.writeConfig(paths, "      groups: [team]\n")
			out, err := f.run()
			if err == nil {
				t.Fatalf("unrecovered failure accepted: %s", out)
			}
			wantComplete, wantDirect := float64(2), 1
			if failure == "repository" || failure == "acquisition" {
				wantComplete = 1
			}
			if failure == "walk-projects" {
				wantDirect = 3
			}
			s := f.summary()
			counts := s["counts"].(map[string]any)
			if s["discovery_complete"] != (failure == "acquisition") || counts["failed"] != float64(1) || counts["complete"] != wantComplete || recursive != 4 || direct != wantDirect {
				t.Fatalf("recursive=%d direct=%d summary=%#v", recursive, direct, s)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitLabAutoFallbackRequiresConsecutive500(t *testing.T) {
	for _, tc := range []struct {
		name              string
		statuses          []int
		success, fallback bool
		invalidDelay      bool
	}{
		{name: "success", statuses: []int{200}, success: true},
		{name: "retry-success", statuses: []int{500, 200}, success: true},
		{name: "mixed", statuses: []int{500, 502, 500}},
		{name: "late-pair", statuses: []int{502, 500, 500}, success: true, fallback: true},
		{name: "unauthorized", statuses: []int{401}},
		{name: "forbidden", statuses: []int{403}},
		{name: "missing", statuses: []int{404}},
		{name: "rate-limit", statuses: []int{429, 429, 429}},
		{name: "unavailable", statuses: []int{503, 503, 503}},
		{name: "transport", statuses: []int{0, 0, 0}},
		{name: "interrupted-pair", statuses: []int{500, 0, 500}},
		{name: "invalid-delay", statuses: []int{500, 500}, invalidDelay: true},
		{name: "malformed", statuses: []int{200}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			calls, walks := 0, 0
			gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("include_subgroups") != "true" {
					walks++
					fmt.Fprint(w, "[]")
					return
				}
				status := tc.statuses[min(calls, len(tc.statuses)-1)]
				calls++
				if status == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				if tc.invalidDelay && calls == 2 {
					w.Header().Set("Retry-After", "test-secret")
				}
				if status != 200 {
					http.Error(w, "test-secret provider body", status)
					return
				}
				if tc.name == "malformed" {
					fmt.Fprint(w, "test-secret")
					return
				}
				fmt.Fprint(w, "[]")
			})
			f.writeConfig("", "      groups: [team]\n    gitlab_group_discovery: auto\n")
			out, err := f.run()
			if (err == nil) != tc.success || calls != len(tc.statuses) || (walks > 0) != tc.fallback || f.summary()["discovery_complete"] != tc.success {
				t.Fatalf("calls=%d walks=%d err=%v %s", calls, walks, err, out)
			}
			if strings.Contains(string(out), "switching to explicit subgroup walking") != tc.fallback {
				t.Fatalf("diagnostic: %s", out)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitLabAutoSwitchDiagnosticRedactsConnectionAndGroup(t *testing.T) {
	f := newFixture(t)
	gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("include_subgroups") == "true" {
			http.Error(w, "test-secret provider body", 500)
			return
		}
		fmt.Fprint(w, "[]")
	})
	f.writeConfig("", "      groups: [test-secret]\n")
	data, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, []byte(strings.Replace(string(data), "name: team", "name: test-secret", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := f.run()
	if err != nil {
		t.Fatalf("fallback: %v %s", err, out)
	}
	if !strings.Contains(string(out), `connection "[REDACTED]" group "[REDACTED]": switching`) {
		t.Fatalf("missing redacted switch: %s", out)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitLabAutoCancellationDuringRetryDoesNotWalk(t *testing.T) {
	f := newFixture(t)
	started := make(chan struct{})
	var calls atomic.Int32
	gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		} else {
			t.Errorf("unexpected request after cancellation: %s", r.URL)
		}
		w.Header().Set("Retry-After", "30")
		http.Error(w, "test-secret", 500)
	})
	f.writeConfig("", "      groups: [team]\n")
	f.syncServer()
	cmd := exec.Command(binary, "--config", f.config)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("discovery never started")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not stop retry")
	}
	f.syncServer()
	if f.summary()["discovery_complete"] != false || strings.Contains(output.String(), "switching to explicit subgroup walking") {
		t.Fatalf("cancellation: %s", output.Bytes())
	}
	f.assertNoSecretOrCheckout(output.Bytes())
}

func TestGitLabAutoDiagnosticRedactsBeforeQuoting(t *testing.T) {
	f := newFixture(t)
	secret := `test-"secret`
	t.Setenv("SWEEP_TEST_TOKEN", secret)
	f.setHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != secret {
			t.Error("missing token")
		}
		if r.URL.Query().Get("include_subgroups") == "true" {
			http.Error(w, secret, 500)
			return
		}
		fmt.Fprint(w, "[]")
	}))
	f.writeConfig("", "      groups: ['"+secret+"']\n")
	out, err := f.run()
	if err != nil {
		t.Fatalf("fallback: %v %s", err, out)
	}
	if !strings.Contains(string(out), `group "[REDACTED]": switching`) || strings.Contains(string(out), `test-\"secret`) {
		t.Fatalf("unredacted quoted group: %s", out)
	}
}
