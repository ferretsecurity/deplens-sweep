package sweep_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gitlabDiscoveryServer(f *fixture, list http.HandlerFunc) {
	f.t.Helper()
	f.server.Close()
	f.server = f.startServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "test-secret" {
			f.t.Error("missing API authentication")
			http.Error(w, "test-secret denied", 401)
			return
		}
		if strings.Contains(r.URL.Path, "/repository/commits/") {
			json.NewEncoder(w).Encode(map[string]string{"id": f.revision})
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/")
		for path, project := range f.projects {
			if p == path || p == fmt.Sprint(project["id"]) {
				json.NewEncoder(w).Encode(project)
				return
			}
		}
		list(w, r)
	}))
	f.t.Cleanup(f.server.Close)
	for _, project := range f.projects {
		project["http_url_to_repo"] = strings.Replace(f.server.URL, "http:", "https:", 1) + "/" + project["path_with_namespace"].(string) + ".git"
	}
}

func gitlabProjectFixture(f *fixture, id int, path string) map[string]any {
	return map[string]any{"id": id, "path_with_namespace": path, "default_branch": "main", "http_url_to_repo": strings.Replace(f.server.URL, "http:", "https:", 1) + "/" + path + ".git", "empty_repo": false}
}

func TestGitLabGroupDiscoversNestedProjects(t *testing.T) {
	f := newFixture(t)
	f.projects["team/subgroup/nested"] = gitlabProjectFixture(f, 43, "team/subgroup/nested")
	gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/groups/team/projects" || r.URL.Query().Get("include_subgroups") != "true" || r.URL.Query().Get("with_shared") != "false" {
			t.Errorf("recursive group query: %s", r.URL)
			http.Error(w, "unexpected query", 400)
			return
		}
		json.NewEncoder(w).Encode([]any{f.projects["team/project"], f.projects["team/subgroup/nested"]})
	})
	f.writeConfig("", "      groups: [team]\n")
	out, err := f.run()
	if err != nil {
		t.Fatalf("group scan: %v %s", err, out)
	}
	results := f.summary()["repositories"].([]any)
	if len(results) != 2 {
		t.Fatalf("selected repositories: %#v", results)
	}
	for _, result := range results {
		item := result.(map[string]any)
		if item["outcome"] != "complete" {
			t.Fatalf("outcome: %#v", item)
		}
		if _, err := os.Stat(filepath.Join(f.results, item["result"].(string), "scan.json")); err != nil {
			t.Fatal(err)
		}
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitLabGroupExclusionsWinOverExactInclusions(t *testing.T) {
	f := newFixture(t)
	f.projects = map[string]map[string]any{}
	for i, path := range []string{"engineering/lib", "engineering/secret/sub/project", "engineering/secret-other/project", "engineering/excluded", "engineering/archived", "engineering/fork"} {
		f.projects[path] = gitlabProjectFixture(f, 50+i, path)
	}
	f.projects["engineering/archived"]["archived"] = true
	f.projects["engineering/fork"]["forked_from_project"] = map[string]any{"id": 1}
	gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/groups/engineering/projects" {
			t.Errorf("excluded group or unexpected request: %s", r.URL)
			http.Error(w, "excluded group unavailable", 404)
			return
		}
		projects := []any{}
		for _, project := range f.projects {
			projects = append(projects, project)
		}
		json.NewEncoder(w).Encode(projects)
	})
	f.writeConfig("  - engineering/lib\n  - engineering/secret/sub/project\n", "      groups: [engineering, engineering, engineering/secret]\n    exclude:\n      groups: [engineering/secret]\n      repositories: [engineering/excluded]\n")
	out, err := f.run()
	if err != nil {
		t.Fatalf("scan: %v %s", err, out)
	}
	results := f.summary()["repositories"].([]any)
	if len(results) != 2 {
		t.Fatalf("selected repositories: %#v", results)
	}
	for _, result := range results {
		path := result.(map[string]any)["path"]
		if path != "engineering/lib" && path != "engineering/secret-other/project" {
			t.Fatalf("excluded project scanned: %s", path)
		}
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if strings.Count(string(log), "clone\n") != 2 {
		t.Fatalf("deduplication failed: %s", log)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitLabGroupPaginationFindsNewRepositoriesOnLaterBatches(t *testing.T) {
	for _, style := range []string{"link", "next-page"} {
		t.Run(style, func(t *testing.T) {
			f := newFixture(t)
			f.projects["team/sub/project"] = gitlabProjectFixture(f, 42, "team/sub/project")
			f.projects["team/sub/deep/project"] = gitlabProjectFixture(f, 43, "team/sub/deep/project")
			f.projects["team/sub/new-project"] = gitlabProjectFixture(f, 44, "team/sub/new-project")
			added := false
			gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.EscapedPath() != "/api/v4/groups/team%2Fsub/projects" {
					t.Errorf("group path not escaped: %s", r.URL)
					http.Error(w, "bad group path", 404)
					return
				}
				if r.URL.Query().Get("page") != "2" {
					if style == "link" {
						w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2&include_subgroups=true&with_shared=false&per_page=100>; rel="next"`, f.server.URL, r.URL.EscapedPath()))
					} else {
						w.Header().Set("X-Next-Page", "2")
					}
					json.NewEncoder(w).Encode([]any{f.projects["team/sub/project"]})
					return
				}
				projects := []any{f.projects["team/sub/deep/project"], f.projects["team/sub/project"]}
				if added {
					projects = append(projects, f.projects["team/sub/new-project"])
				}
				json.NewEncoder(w).Encode(projects)
			})
			f.writeConfig("", "      groups: [team/sub]\n")
			out, err := f.run()
			if err != nil {
				t.Fatalf("first scan: %v %s", err, out)
			}
			if len(f.summary()["repositories"].([]any)) != 2 {
				t.Fatalf("later page omitted: %#v", f.summary())
			}
			added = true
			out, err = f.run()
			if err != nil {
				t.Fatalf("later batch: %v %s", err, out)
			}
			summary := f.summary()
			counts := summary["counts"].(map[string]any)
			if len(summary["repositories"].([]any)) != 3 || counts["complete"] != float64(1) || counts["reused"] != float64(2) {
				t.Fatalf("newly discovered repository omitted: %#v", summary)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitLabAllAccessibleRequiresOptInAndHonorsFilters(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint("archive-and-forks-", enabled), func(t *testing.T) {
			f := newFixture(t)
			for i, path := range []string{"team/archived", "team/fork", "private/sub/project", "team/excluded", "private-other/project"} {
				f.projects[path] = gitlabProjectFixture(f, 43+i, path)
			}
			f.projects["team/archived"]["archived"] = true
			f.projects["team/fork"]["forked_from_project"] = map[string]any{"id": 1}
			gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v4/projects" {
					t.Errorf("all-accessible API: %s", r.URL)
					http.Error(w, "unexpected API", 400)
					return
				}
				if r.URL.Query().Get("page") == "2" {
					json.NewEncoder(w).Encode([]any{f.projects["private-other/project"], f.projects["team/project"]})
					return
				}
				w.Header().Set("X-Next-Page", "2")
				projects := []any{}
				for _, project := range f.projects {
					projects = append(projects, project)
				}
				json.NewEncoder(w).Encode(projects)
			})
			f.writeConfig("", "    all_accessible: false\n")
			out, err := f.run()
			if err == nil || !strings.Contains(string(out), "explicit repository selection") {
				t.Fatalf("implicit all-accessible accepted: %v %s", err, out)
			}
			extra := "    all_accessible: true\n    exclude:\n      groups: [private]\n      repositories: [team/excluded]\n"
			if enabled {
				extra += "    include_archived: true\n    include_forks: true\n"
			}
			f.writeConfig("", extra)
			out, err = f.run()
			if err != nil {
				t.Fatalf("all-accessible scan: %v %s", err, out)
			}
			results := f.summary()["repositories"].([]any)
			want := 2
			if enabled {
				want = 4
			}
			if len(results) != want {
				t.Fatalf("selected repositories: %#v", results)
			}
			for _, result := range results {
				path := result.(map[string]any)["path"].(string)
				if path == "team/excluded" || strings.HasPrefix(path, "private/") {
					t.Fatalf("excluded project scanned: %s", path)
				}
			}
			selection := f.summary()["selections"].([]any)[0].(map[string]any)
			if selection["all_accessible"] != true {
				t.Fatalf("all-accessible opt-in absent from batch: %#v", selection)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitLabLaterPageAndNestedGroupFailuresKeepKnownWork(t *testing.T) {
	f := newFixture(t)
	f.projects["other/project"] = gitlabProjectFixture(f, 43, "other/project")
	gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/groups/team/projects" && r.URL.Query().Get("page") != "2" {
			w.Header().Set("X-Next-Page", "2")
			json.NewEncoder(w).Encode([]any{f.projects["team/project"]})
			return
		}
		if r.URL.Path == "/api/v4/groups/team/projects" {
			http.Error(w, "test-secret transient server failure", 500)
			return
		}
		if r.URL.EscapedPath() == "/api/v4/groups/team%2Fmissing/projects" {
			http.Error(w, "test-secret inaccessible subgroup", 404)
			return
		}
		t.Errorf("unexpected request: %s", r.URL)
		http.Error(w, "unexpected", 400)
	})
	f.writeConfig("  - other/project\n", "      groups: [team, team/missing]\n")
	out, err := f.run()
	if err == nil {
		t.Fatalf("incomplete discovery accepted: %s", out)
	}
	summary := f.summary()
	counts := summary["counts"].(map[string]any)
	if summary["discovery_complete"] != false || counts["failed"] != float64(2) || counts["complete"] != float64(2) {
		t.Fatalf("lost known projects or discovery errors: %#v", summary)
	}
	completed := map[string]bool{}
	for _, result := range summary["repositories"].([]any) {
		item := result.(map[string]any)
		if item["outcome"] == "complete" {
			completed[item["path"].(string)] = true
		}
	}
	if !completed["other/project"] || !completed["team/project"] {
		t.Fatalf("known work missing: %#v", completed)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitLabPaginationRejectsUntrustedURLsAndLoops(t *testing.T) {
	for _, mode := range []string{"other-server", "credentials", "fragment", "outside-api", "loop", "invalid-page", "invalid-array"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			destination := httptest.NewServer(f.wrapHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("contacted untrusted pagination server")
				if r.Header.Get("PRIVATE-TOKEN") != "" {
					t.Error("token forwarded")
				}
			})))
			t.Cleanup(destination.Close)
			gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
				next := f.server.URL + r.URL.RequestURI()
				switch mode {
				case "other-server":
					next = destination.URL + "/api/v4/projects"
				case "credentials":
					next = strings.Replace(next, "://", "://user:test-secret@", 1)
				case "fragment":
					next += "#test-secret"
				case "outside-api":
					next = f.server.URL + "/elsewhere"
				case "invalid-page":
					w.Header().Set("X-Next-Page", "garbage")
				case "invalid-array":
					fmt.Fprint(w, "null")
					return
				}
				if mode != "invalid-page" {
					w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
				}
				json.NewEncoder(w).Encode([]any{f.projects["team/project"]})
			})
			f.writeConfig("  - team/project\n", "      groups: [team]\n")
			out, err := f.run()
			if err == nil {
				t.Fatalf("invalid pagination accepted: %s", out)
			}
			summary := f.summary()
			counts := summary["counts"].(map[string]any)
			if summary["discovery_complete"] != false || counts["failed"] != float64(1) || counts["complete"] != float64(1) {
				t.Fatalf("summary: %#v", summary)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitLabRejectsWildcardsAndUnsupportedProviderSelections(t *testing.T) {
	cases := []struct {
		name, paths, extra string
		github             bool
	}{
		{name: "wildcard group", extra: "      groups: ['team/*']\n"},
		{name: "wildcard exclusion", paths: "  - team/project\n", extra: "    exclude:\n      groups: ['team/*']\n"},
		{name: "github group", paths: "  - team/project\n", extra: "      groups: [team]\n", github: true},
		{name: "github group exclusion", paths: "  - team/project\n", extra: "    exclude:\n      groups: [team]\n", github: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.writeConfig(tc.paths, tc.extra)
			if tc.github {
				data, _ := os.ReadFile(f.config)
				os.WriteFile(f.config, []byte(strings.Replace(string(data), "provider: gitlab", "provider: github", 1)), 0600)
			}
			out, err := f.run()
			if err == nil {
				t.Fatalf("invalid selection accepted: %s", out)
			}
			if _, err := os.Stat(f.results); !os.IsNotExist(err) {
				t.Fatalf("invalid selection created batch output: %v", err)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitLabEmptyCurrentSelectionKeepsHistoryOutOfBatch(t *testing.T) {
	f := newFixture(t)
	selected := true
	gitlabDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		projects := []any{}
		if selected {
			projects = append(projects, f.projects["team/project"])
		}
		json.NewEncoder(w).Encode(projects)
	})
	f.writeConfig("", "      groups: [team]\n")
	out, err := f.run()
	if err != nil {
		t.Fatalf("first scan: %v %s", err, out)
	}
	prior := f.summary()["repositories"].([]any)[0].(map[string]any)["result"].(string)
	selected = false
	out, err = f.run()
	if err != nil {
		t.Fatalf("empty selection batch: %v %s", err, out)
	}
	summary := f.summary()
	if len(summary["repositories"].([]any)) != 0 || summary["discovery_complete"] != true {
		t.Fatalf("history included in current batch: %#v", summary)
	}
	if _, err := os.Stat(filepath.Join(f.results, prior, "scan.json")); err != nil {
		t.Fatal("deselection removed history:", err)
	}
	f.assertNoSecretOrCheckout(out)
}
