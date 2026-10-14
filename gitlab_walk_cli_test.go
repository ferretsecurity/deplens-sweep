package sweep_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestGitLabWalkNestedPaginatedGroups(t *testing.T) {
	for _, style := range []string{"link", "next-page"} {
		t.Run(style, func(t *testing.T) {
			f := newFixture(t)
			for i, p := range []string{"team/sub/one", "team/sub/deep/two", "team/sub/three"} {
				f.projects[p] = gitlabProjectFixture(f, 50+i, p)
			}
			calls := map[string]int{}
			gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
				calls[r.URL.RequestURI()]++
				if !strings.Contains(r.URL.EscapedPath(), "team%2Fsub") {
					t.Errorf("unescaped group: %s", r.URL)
				}
				if r.URL.Query().Get("per_page") != "100" {
					t.Errorf("page size: %s", r.URL)
				}
				next := func() {
					if style == "link" {
						q := r.URL.Query()
						q.Set("page", "2")
						w.Header().Set("Link", fmt.Sprintf(`<%s?%s>; rel="next"`, r.URL.EscapedPath(), q.Encode()))
					} else {
						w.Header().Set("X-Next-Page", "2")
					}
				}
				items := []any{}
				switch r.URL.Path {
				case "/api/v4/groups/team/sub/projects":
					if r.URL.Query().Get("page") == "2" {
						items = append(items, f.projects["team/sub/three"])
					} else {
						next()
						items = append(items, f.projects["team/sub/one"])
					}
				case "/api/v4/groups/team/sub/subgroups":
					if r.URL.Query().Get("page") == "2" {
						items = append(items, map[string]any{"id": 3, "full_path": "team/sub/empty"})
					} else {
						next()
						items = append(items, map[string]any{"id": 2, "full_path": "team/sub/deep"}, map[string]any{"id": 4, "full_path": "team/sub/secret"})
					}
				case "/api/v4/groups/team/sub/deep/projects":
					items = append(items, f.projects["team/sub/deep/two"])
				case "/api/v4/groups/team/sub/deep/subgroups", "/api/v4/groups/team/sub/empty/projects", "/api/v4/groups/team/sub/empty/subgroups":
				default:
					t.Errorf("unexpected traversal: %s", r.URL)
				}
				if strings.HasSuffix(r.URL.Path, "/projects") && (r.URL.Query().Get("include_subgroups") != "false" || r.URL.Query().Get("with_shared") != "false") {
					t.Errorf("direct query: %s", r.URL)
				}
				json.NewEncoder(w).Encode(items)
			})
			f.writeConfig("  - team/sub/one\n", "      groups: [team/sub, team/sub/deep, team/sub]\n    gitlab_group_discovery: walk\n    exclude:\n      groups: [team/sub/secret]\n")
			out, err := f.run()
			if err != nil {
				t.Fatalf("walk: %v %s", err, out)
			}
			s := f.summary()
			if s["discovery_complete"] != true || s["counts"].(map[string]any)["complete"] != float64(3) {
				t.Fatalf("summary: %#v", s)
			}
			for p, n := range calls {
				if n != 1 {
					t.Errorf("repeated group request %s: %d", p, n)
				}
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitLabRecursiveRetryBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []int
		want     int
		success  bool
	}{
		{"two-500", []int{500, 500}, 2, false}, {"recover", []int{500, 200}, 2, true}, {"mixed", []int{500, 502, 500}, 3, false}, {"late-pair", []int{502, 500, 500}, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			calls := 0
			gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("include_subgroups") != "true" {
					t.Errorf("unexpected walk: %s", r.URL)
				}
				status := tc.statuses[min(calls, len(tc.statuses)-1)]
				calls++
				if status != 200 {
					http.Error(w, "test-secret", status)
					return
				}
				json.NewEncoder(w).Encode([]any{f.projects["team/project"]})
			})
			f.writeConfig("", "      groups: [team]\n    gitlab_group_discovery: recursive\n")
			out, err := f.run()
			if (err == nil) != tc.success || calls != tc.want {
				t.Fatalf("calls=%d want=%d error=%v output=%s", calls, tc.want, err, out)
			}
			if f.summary()["discovery_complete"] != tc.success {
				t.Fatalf("summary: %#v", f.summary())
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitLabWalkKeepsKnownBranchesAfterListFailures(t *testing.T) {
	f := newFixture(t)
	f.projects["team/sub/project"] = gitlabProjectFixture(f, 52, "team/sub/project")
	calls := map[string]int{}
	gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		calls[r.URL.RequestURI()]++
		switch r.URL.Path {
		case "/api/v4/groups/team/projects":
			http.Error(w, "test-secret", 500)
		case "/api/v4/groups/team/subgroups":
			if r.URL.Query().Get("page") == "2" {
				http.Error(w, "test-secret", 403)
				return
			}
			w.Header().Set("X-Next-Page", "2")
			json.NewEncoder(w).Encode([]any{map[string]any{"id": 2, "full_path": "team/sub"}})
		case "/api/v4/groups/team/sub/projects":
			json.NewEncoder(w).Encode([]any{f.projects["team/sub/project"]})
		case "/api/v4/groups/team/sub/subgroups":
			fmt.Fprint(w, "[]")
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.Error(w, "unexpected", 400)
		}
	})
	f.writeConfig("  - team/project\n", "      groups: [team]\n    gitlab_group_discovery: walk\n")
	out, err := f.run()
	if err == nil {
		t.Fatal("incomplete discovery accepted")
	}
	s := f.summary()
	counts := s["counts"].(map[string]any)
	if s["discovery_complete"] != false || counts["complete"] != float64(2) || counts["failed"] != float64(2) {
		t.Fatalf("summary: %#v", s)
	}
	if calls["/api/v4/groups/team/projects?include_subgroups=false&with_shared=false&per_page=100"] != 3 {
		t.Fatalf("walk retry budget: %#v", calls)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitLabDiscoveryModeValidation(t *testing.T) {
	for _, tc := range []struct {
		mode, provider string
		valid          bool
	}{
		{"walk", "gitlab", true}, {"recursive", "gitlab", true}, {"", "gitlab", true}, {"auto", "gitlab", true}, {"invalid", "gitlab", false}, {"walk", "github", false}, {"recursive", "github", false}, {"auto", "github", false},
	} {
		t.Run(tc.provider+"-"+tc.mode, func(t *testing.T) {
			f := newFixture(t)
			f.writeConfig("  - team/project\n", "    gitlab_group_discovery: '"+tc.mode+"'\n")
			if tc.provider == "github" {
				data, _ := os.ReadFile(f.config)
				os.WriteFile(f.config, []byte(strings.Replace(string(data), "provider: gitlab", "provider: github", 1)), 0600)
			}
			out, err := f.run()
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v %s", tc.valid, err, out)
			}
			if !tc.valid {
				if _, err := os.Stat(f.results); !os.IsNotExist(err) {
					t.Fatalf("invalid mode created output: %v", err)
				}
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitLabWalkSubgroupPaginationRejectsUnsafeResponses(t *testing.T) {
	for _, mode := range []string{"outside-api", "credentials", "fragment", "loop", "invalid-page", "invalid-array", "invalid-identity", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/projects") {
					json.NewEncoder(w).Encode([]any{f.projects["team/project"]})
					return
				}
				next := f.server.URL + r.URL.RequestURI()
				switch mode {
				case "outside-api":
					next = f.server.URL + "/elsewhere"
				case "credentials":
					next = strings.Replace(next, "://", "://user:test-secret@", 1)
				case "fragment":
					next += "#test-secret"
				case "invalid-page":
					w.Header().Set("X-Next-Page", "bad")
					fmt.Fprint(w, "[]")
					return
				case "invalid-array":
					fmt.Fprint(w, "null")
					return
				case "invalid-identity":
					fmt.Fprint(w, `[{"id":0}]`)
					return
				case "redirect":
					w.Header().Set("Location", f.server.URL+"/elsewhere")
					w.WriteHeader(302)
					return
				}
				w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
				fmt.Fprint(w, "[]")
			})
			f.writeConfig("", "      groups: [team]\n    gitlab_group_discovery: walk\n")
			out, err := f.run()
			if err == nil {
				t.Fatal("invalid subgroup listing accepted")
			}
			s := f.summary()
			if s["discovery_complete"] != false || s["counts"].(map[string]any)["complete"] != float64(1) || s["counts"].(map[string]any)["failed"] != float64(1) {
				t.Fatalf("summary: %#v", s)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitLabWalkCancellationStopsTraversal(t *testing.T) {
	f := newFixture(t)
	started := make(chan struct{})
	var calls atomic.Int32
	gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		} else {
			t.Errorf("request after cancellation: %s", r.URL)
		}
		<-r.Context().Done()
	})
	f.writeConfig("", "      groups: [team, other]\n    gitlab_group_discovery: walk\n")
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
		t.Fatal("cancellation did not stop discovery")
	}
	if f.summary()["discovery_complete"] != false {
		t.Fatalf("summary: %#v", f.summary())
	}
	f.assertNoSecretOrCheckout(output.Bytes())
}

func TestGitLabWalkFiltersProjectsUnderInstallationPath(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			f := newFixture(t)
			f.projects = map[string]map[string]any{}
			for i, path := range []string{"team/project", "team/excluded", "team/archived", "team/fork", "team/secret/project", "team/secret-other/project"} {
				f.projects[path] = gitlabProjectFixture(f, 60+i, path)
				f.projects[path]["empty_repo"] = true
			}
			f.projects["team/archived"]["archived"] = true
			f.projects["team/fork"]["forked_from_project"] = map[string]any{"id": 1}
			f.setHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("PRIVATE-TOKEN") != "test-secret" {
					t.Error("missing token")
				}
				switch r.URL.Path {
				case "/gitlab/api/v4/groups/team/projects":
					items := []any{}
					for _, p := range f.projects {
						items = append(items, p)
					}
					json.NewEncoder(w).Encode(items)
				case "/gitlab/api/v4/groups/team/subgroups":
					fmt.Fprint(w, "[]")
				default:
					t.Errorf("installation path: %s", r.URL)
					http.Error(w, "unexpected", 400)
				}
			}))
			f.writeConfig("", "      groups: [team]\n    gitlab_group_discovery: walk\n    include_archived: "+fmt.Sprint(enabled)+"\n    include_forks: "+fmt.Sprint(enabled)+"\n    exclude:\n      repositories: [team/excluded]\n      groups: [team/secret]\n")
			data, _ := os.ReadFile(f.config)
			os.WriteFile(f.config, []byte(strings.Replace(string(data), f.server.URL, f.server.URL+"/gitlab", 1)), 0600)
			out, err := f.run()
			if err != nil {
				t.Fatalf("walk: %v %s", err, out)
			}
			want := float64(2)
			if enabled {
				want = 4
			}
			if f.summary()["counts"].(map[string]any)["empty"] != want {
				t.Fatalf("filters: %#v", f.summary())
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitLabRecursiveSecond500RejectsInvalidRetryDelay(t *testing.T) {
	f := newFixture(t)
	calls := 0
	gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			w.Header().Set("Retry-After", "test-secret")
		}
		http.Error(w, "test-secret", 500)
	})
	f.writeConfig("", "      groups: [team]\n    gitlab_group_discovery: recursive\n")
	out, err := f.run()
	if err == nil || calls != 2 || !strings.Contains(string(out), "invalid server retry delay") {
		t.Fatalf("calls=%d err=%v %s", calls, err, out)
	}
	f.assertNoSecretOrCheckout(out)
}
