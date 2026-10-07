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

func newGitHubFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.server.Close()
	f.projects = map[string]map[string]any{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" || r.Header.Get("PRIVATE-TOKEN") != "" {
			t.Errorf("missing GitHub API authentication")
			http.Error(w, "test-secret forbidden", 401)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/api/v3/repos/")
		if strings.Contains(p, "/commits/") {
			json.NewEncoder(w).Encode(map[string]string{"sha": f.revision})
			return
		}
		if project, ok := f.projects[p]; ok {
			json.NewEncoder(w).Encode(project)
			return
		}
		http.Error(w, "test-secret repository unavailable", 404)
	}))
	t.Cleanup(f.server.Close)
	f.projects["team/project"] = map[string]any{"id": 42, "full_name": "team/project", "default_branch": "main", "clone_url": strings.Replace(f.server.URL, "http:", "https:", 1) + "/team/project.git"}
	writeGitHubConfig(f, "  - team/project\n", "")
	return f
}

func writeGitHubConfig(f *fixture, paths, extra string) {
	f.t.Helper()
	f.writeConfig(paths, extra)
	data, err := os.ReadFile(f.config)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = os.WriteFile(f.config, []byte(strings.Replace(string(data), "provider: gitlab", "provider: github", 1)), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func TestGitHubExactRepositoryProducesInventoryAndMetadata(t *testing.T) {
	f := newGitHubFixture(t)
	out, err := f.run()
	if err != nil {
		t.Fatalf("GitHub scan: %v %s", err, out)
	}
	summary := f.summary()
	results := summary["repositories"].([]any)
	if len(results) != 1 {
		t.Fatalf("repositories: %#v", results)
	}
	result := results[0].(map[string]any)
	if result["outcome"] != "complete" || result["provider"] != "github" || result["repository_id"] != "42" {
		t.Fatalf("outcome: %#v", result)
	}
	ref := result["result"].(string)
	data, err := os.ReadFile(filepath.Join(f.results, ref, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err = json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["provider"] != "github" || metadata["server_url"] != f.server.URL || metadata["revision"] != f.revision || metadata["default_branch"] != "main" {
		t.Fatalf("metadata: %s", data)
	}
	if _, err = os.Stat(filepath.Join(f.results, ref, "scan.json")); err != nil {
		t.Fatal(err)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitHubConfirmedEmptyIsSeparateFromInaccessibleRepository(t *testing.T) {
	f := newGitHubFixture(t)
	f.server.Close()
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("missing authentication")
		}
		switch r.URL.Path {
		case "/api/v3/repos/team/empty", "/api/v3/repos/team/unavailable":
			json.NewEncoder(w).Encode(map[string]any{"id": map[bool]int{true: 7, false: 8}[strings.HasSuffix(r.URL.Path, "empty")], "full_name": strings.TrimPrefix(r.URL.Path, "/api/v3/repos/"), "default_branch": "main"})
		case "/api/v3/repos/team/empty/commits/main":
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]string{"message": "Git Repository is empty."})
		case "/api/v3/repos/team/unavailable/commits/main":
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]string{"message": "test-secret repository unavailable"})
		case "/api/v3/repos/team/project":
			json.NewEncoder(w).Encode(f.projects["team/project"])
		case "/api/v3/repos/team/project/commits/main":
			json.NewEncoder(w).Encode(map[string]string{"sha": f.revision})
		default:
			http.Error(w, "test-secret inaccessible", 404)
		}
	}))
	t.Cleanup(f.server.Close)
	f.projects["team/project"]["clone_url"] = strings.Replace(f.server.URL, "http:", "https:", 1) + "/team/project.git"
	writeGitHubConfig(f, "  - team/empty\n  - team/unavailable\n  - team/missing\n  - team/project\n", "")
	out, err := f.run()
	if err == nil {
		t.Fatalf("inaccessible repositories accepted: %s", out)
	}
	summary := f.summary()
	counts := summary["counts"].(map[string]any)
	if counts["empty"] != float64(1) || counts["failed"] != float64(2) || counts["complete"] != float64(1) || summary["discovery_complete"] != false {
		t.Fatalf("summary: %#v", summary)
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if strings.Count(string(log), "clone\n") != 1 {
		t.Fatalf("empty or inaccessible cloned: %s", log)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitHubSelectionsDeduplicateAndHonorFilters(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "enabled"}[enabled], func(t *testing.T) {
			f := newGitHubFixture(t)
			project := f.projects["team/project"]
			f.projects["team/old-name"] = project
			for _, name := range []string{"archived", "fork", "excluded", "canonical-exclusion"} {
				f.projects["team/"+name] = map[string]any{"id": map[string]int{"archived": 43, "fork": 44, "excluded": 45, "canonical-exclusion": 46}[name], "full_name": "team/" + name, "default_branch": "main", "clone_url": project["clone_url"]}
			}
			f.projects["team/archived"]["archived"] = true
			f.projects["team/fork"]["fork"] = true
			f.projects["team/alias"] = f.projects["team/canonical-exclusion"]
			extra := "    exclude:\n      repositories: [team/excluded, team/canonical-exclusion]\n"
			if enabled {
				extra += "    include_archived: true\n    include_forks: true\n"
			}
			writeGitHubConfig(f, "  - team/project\n  - team/project\n  - team/old-name\n  - team/archived\n  - team/fork\n  - team/excluded\n  - team/alias\n", extra)
			out, err := f.run()
			if err != nil {
				t.Fatalf("scan: %v %s", err, out)
			}
			want := 1
			if enabled {
				want = 3
			}
			results := f.summary()["repositories"].([]any)
			if len(results) != want {
				t.Fatalf("repositories: %#v", results)
			}
			for _, result := range results {
				path := result.(map[string]any)["path"].(string)
				if path != "team/project" && path != "team/archived" && path != "team/fork" {
					t.Fatalf("unexpected repository: %s", path)
				}
			}
			log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
			if strings.Count(string(log), "clone\n") != want {
				t.Fatalf("duplicate clone: %s", log)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitHubDefaultBranchWithSlashPinsResolvedCommit(t *testing.T) {
	f := newGitHubFixture(t)
	f.projects["team/project"]["default_branch"] = "release/stable"
	mustRun(t, "/usr/bin/git", "-C", f.repo, "branch", "release/stable")
	mustRun(t, "/usr/bin/git", "-C", f.repo, "checkout", "release/stable")
	os.WriteFile(filepath.Join(f.repo, "requirements.txt"), []byte("flask==3.1.0\n"), 0600)
	mustRun(t, "/usr/bin/git", "-C", f.repo, "add", ".")
	mustRun(t, "/usr/bin/git", "-C", f.repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "branch advanced")
	gitPath := filepath.Join(f.dir, "bin", "git")
	data, _ := os.ReadFile(gitPath)
	os.WriteFile(gitPath, []byte(strings.ReplaceAll(string(data), "--branch main", "--branch release/stable")), 0700)
	f.server.Close()
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("missing API authentication")
		}
		switch r.URL.EscapedPath() {
		case "/api/v3/repos/team/project":
			json.NewEncoder(w).Encode(f.projects["team/project"])
		case "/api/v3/repos/team/project/commits/release%2Fstable":
			json.NewEncoder(w).Encode(map[string]string{"sha": f.revision})
		default:
			http.Error(w, "unexpected path", 404)
		}
	}))
	t.Cleanup(f.server.Close)
	f.projects["team/project"]["clone_url"] = strings.Replace(f.server.URL, "http:", "https:", 1) + "/team/project.git"
	writeGitHubConfig(f, "  - team/project\n", "")
	os.WriteFile(f.scanner, []byte("#!/bin/sh\nfor arg in \"$@\"; do checkout=\"$arg\"; done\ngrep -q 'requests==2.32.3' \"$checkout/requirements.txt\" || exit 8\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"fixture\",\"sources\":[],\"check_runs\":[],\"findings\":[]}'\n"), 0700)
	out, err := f.run()
	if err != nil {
		t.Fatalf("resolved slash-branch revision not scanned: %v %s", err, out)
	}
	result := f.summary()["repositories"].([]any)[0].(map[string]any)
	if result["revision"] != f.revision || result["default_branch"] != "release/stable" {
		t.Fatalf("result: %#v", result)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitHubPublicAPIRoutesSeparatelyFromCloneHost(t *testing.T) {
	f := newGitHubFixture(t)
	publicGitHubProxy(f, func(request *http.Request) (any, string) {
		switch request.URL.Path {
		case "/repos/team/project":
			return map[string]any{"id": 42, "full_name": "team/project", "default_branch": "main", "clone_url": "https://github.com/team/project.git"}, ""
		case "/repos/team/project/commits/main":
			return map[string]string{"sha": f.revision}, ""
		default:
			t.Errorf("unexpected public API path: %s", request.URL.Path)
			return map[string]string{}, ""
		}
	})
	data, _ := os.ReadFile(f.config)
	os.WriteFile(f.config, []byte(strings.ReplaceAll(string(data), f.server.URL, "https://github.com:443/")), 0600)
	out, err := f.run()
	if err != nil {
		t.Fatalf("public GitHub scan: %v %s", err, out)
	}
	result := f.summary()["repositories"].([]any)[0].(map[string]any)
	if result["outcome"] != "complete" {
		t.Fatalf("result: %#v", result)
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if !strings.Contains(string(log), "https://github.com/team/project.git") {
		t.Fatalf("public clone URL: %s", log)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitHubErrorsDoNotBecomeEmptyOrExposeCredentials(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		status int
		raw    string
		commit bool
	}{
		{name: "unauthenticated", status: 401},
		{name: "forbidden", status: 403},
		{name: "missing", status: 404},
		{name: "lookup conflict", status: 409, raw: `{"message":"Git Repository is empty."}`},
		{name: "invalid JSON", raw: `test-secret`},
		{name: "missing identity", mutate: func(p map[string]any) { delete(p, "id") }},
		{name: "no default branch", mutate: func(p map[string]any) { delete(p, "default_branch") }},
		{name: "missing commit", status: 404, commit: true},
		{name: "invalid commit", raw: `{"sha":"test-secret"}`, commit: true},
		{name: "clone with credentials", mutate: func(p map[string]any) { p["clone_url"] = "https://test-secret@github.example/team/project.git" }},
		{name: "clone from another server", mutate: func(p map[string]any) { p["clone_url"] = "https://another.example/team/project.git" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGitHubFixture(t)
			if tc.mutate != nil {
				tc.mutate(f.projects["team/project"])
			}
			f.server.Close()
			f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-secret" {
					t.Error("missing API authentication")
				}
				commit := strings.Contains(r.URL.Path, "/commits/")
				if commit == tc.commit && (tc.status != 0 || tc.raw != "") {
					status := tc.status
					if status == 0 {
						status = 200
					}
					w.WriteHeader(status)
					if tc.raw != "" {
						fmt.Fprint(w, tc.raw)
					} else {
						fmt.Fprint(w, `{"message":"test-secret error"}`)
					}
					return
				}
				if commit {
					json.NewEncoder(w).Encode(map[string]string{"sha": f.revision})
					return
				}
				json.NewEncoder(w).Encode(f.projects["team/project"])
			}))
			t.Cleanup(f.server.Close)
			// Keep valid clone addresses on the new configured server.
			if !strings.HasPrefix(tc.name, "clone ") {
				f.projects["team/project"]["clone_url"] = strings.Replace(f.server.URL, "http:", "https:", 1) + "/team/project.git"
			}
			writeGitHubConfig(f, "  - team/project\n", "")
			out, err := f.run()
			if err == nil {
				t.Fatalf("error accepted: %s", out)
			}
			counts := f.summary()["counts"].(map[string]any)
			if counts["failed"] != float64(1) || counts["empty"] != float64(0) {
				t.Fatalf("outcomes: %#v", counts)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitHubAPIRedirectDoesNotForwardToken(t *testing.T) {
	f := newGitHubFixture(t)
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("followed API redirect")
		if r.Header.Get("Authorization") != "" {
			t.Error("token forwarded to redirect destination")
		}
	}))
	t.Cleanup(destination.Close)
	f.server.Close()
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/test-secret", http.StatusFound)
	}))
	t.Cleanup(f.server.Close)
	writeGitHubConfig(f, "  - team/project\n", "")
	out, err := f.run()
	if err == nil {
		t.Fatalf("redirect accepted: %s", out)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitHubFailedScanRetainsHistoryAndCleansCheckout(t *testing.T) {
	f := newGitHubFixture(t)
	out, err := f.run()
	if err != nil {
		t.Fatalf("first scan: %v %s", err, out)
	}
	previous := f.summary()["repositories"].([]any)[0].(map[string]any)["result"].(string)
	os.WriteFile(f.scanner, []byte("#!/bin/sh\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"fixture\",\"sources\":[],\"check_runs\":[],\"findings\":[]}'\nexit 5\n"), 0700)
	out, err = f.run()
	if err == nil {
		t.Fatalf("failed scanner accepted: %s", out)
	}
	result := f.summary()["repositories"].([]any)[0].(map[string]any)
	if result["outcome"] != "failed" || result["result"] == previous {
		t.Fatalf("failed attempt: %#v", result)
	}
	ref := result["result"].(string)
	for _, retained := range []string{previous, ref} {
		for _, filename := range []string{"scan.json", "metadata.json"} {
			if _, err := os.Stat(filepath.Join(f.results, retained, filename)); err != nil {
				t.Fatal(err)
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(f.results, filepath.Dir(filepath.Dir(previous)), "latest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var latest map[string]any
	json.Unmarshal(data, &latest)
	if latest["result"] != previous {
		t.Fatalf("failed result replaced latest complete: %s", data)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitHubRealDeplensProducesDependencyInventory(t *testing.T) {
	scanner := os.Getenv("SWEEP_REAL_DEPLENS")
	if scanner == "" {
		t.Skip("set SWEEP_REAL_DEPLENS to an installed real deplens executable")
	}
	f := newGitHubFixture(t)
	f.scanner = scanner
	writeGitHubConfig(f, "  - team/project\n", "")
	out, err := f.run()
	if err != nil {
		t.Fatalf("real GitHub scan: %v %s", err, out)
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
