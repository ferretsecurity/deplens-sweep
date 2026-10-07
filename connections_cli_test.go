package sweep_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func connectionConfig(f *fixture, connections string) {
	f.t.Helper()
	config := fmt.Sprintf("min_free_bytes: 0\nresults_dir: %s\ntemp_dir: %s\nconnections:\n%sdeplens:\n  executable: %s\n  disable_rules: [shared-rule]\n", f.results, filepath.Join(f.dir, "work"), connections, f.scanner)
	if err := os.WriteFile(f.config, []byte(config), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func TestAccountsOnSameServerKeepSelectionsAndReuseIndependent(t *testing.T) {
	f := newFixture(t)
	t.Setenv("SWEEP_SECOND_TOKEN", "other-secret")
	var mu sync.Mutex
	apiAccounts := map[string]int{}
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("PRIVATE-TOKEN")
		account := map[string]string{"test-secret": "first", "other-secret": "second"}[token]
		if account == "" || r.Header.Get("Authorization") != "" {
			t.Error("wrong account credentials")
			http.Error(w, "denied", 401)
			return
		}
		mu.Lock()
		apiAccounts[account]++
		mu.Unlock()
		if strings.Contains(r.URL.Path, "/repository/commits/") {
			json.NewEncoder(w).Encode(map[string]string{"id": f.revision})
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/")
		id := 42
		if path == "team/extra" {
			id = 43
		}
		json.NewEncoder(w).Encode(map[string]any{"id": id, "path_with_namespace": path, "default_branch": "main", "http_url_to_repo": strings.Replace(f.server.URL, "http:", "https:", 1) + "/" + path + ".git", "empty_repo": false})
	})
	gitPath := filepath.Join(f.dir, "bin", "git")
	raw, _ := os.ReadFile(gitPath)
	raw = []byte(strings.Replace(string(raw), `[ "$("$GIT_ASKPASS" 'Password for https://server')" = test-secret ] || exit 92`, `[ "$("$GIT_ASKPASS" 'Password for https://server')" = "$SWEEP_GIT_TOKEN" ] || exit 92
  case "$SWEEP_GIT_TOKEN" in
    test-secret) echo first >> "$SWEEP_CONNECTION_LOG" ;;
    other-secret) echo second >> "$SWEEP_CONNECTION_LOG" ;;
    *) exit 93 ;;
  esac`, 1))
	os.WriteFile(gitPath, raw, 0700)
	logPath := filepath.Join(f.dir, "connection-clones.log")
	t.Setenv("SWEEP_CONNECTION_LOG", logPath)
	firstConfig := connectionBlock("first", "gitlab", f.server.URL, "SWEEP_TEST_TOKEN", "team/project, team/project, team/extra", "    exclude:\n      repositories: [team/extra]\n")
	connectionConfig(f, firstConfig)
	first := reuseRun(t, f)
	secondConfig := connectionBlock("second", "gitlab", f.server.URL, "SWEEP_SECOND_TOKEN", "team/project, team/extra", "")
	connectionConfig(f, firstConfig+secondConfig)
	if out, err := f.run(); err != nil {
		t.Fatalf("second account batch: %v %s", err, out)
	}
	rows := f.summary()["repositories"].([]any)
	if len(rows) != 3 {
		t.Fatalf("connection selection or deduplication leaked: %#v", rows)
	}
	extraRef := ""
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["connection"] == "first" {
			if row["outcome"] != "reused" || row["result"] != first["result"] || row["path"] != "team/project" {
				t.Fatalf("first account history changed: %#v", row)
			}
		} else {
			if row["outcome"] != "complete" || row["result"] == first["result"] {
				t.Fatalf("new account borrowed old history: %#v", row)
			}
			if row["path"] == "team/extra" {
				extraRef = row["result"].(string)
			}
		}
	}
	clones, _ := os.ReadFile(logPath)
	if strings.Count(string(clones), "first\n") != 1 || strings.Count(string(clones), "second\n") != 2 {
		t.Fatalf("clone credentials or reuse crossed accounts: %s", clones)
	}
	mu.Lock()
	seenFirst, seenSecond := apiAccounts["first"], apiAccounts["second"]
	mu.Unlock()
	if seenFirst == 0 || seenSecond == 0 {
		t.Fatalf("API credentials crossed accounts: %d %d", seenFirst, seenSecond)
	}
	secondExcluded := connectionBlock("second", "gitlab", f.server.URL, "SWEEP_SECOND_TOKEN", "team/project, team/extra", "    exclude:\n      repositories: [team/extra]\n")
	connectionConfig(f, firstConfig+secondExcluded)
	if out, err := f.run(); err != nil {
		t.Fatalf("independent exclusions: %v %s", err, out)
	}
	rows = f.summary()["repositories"].([]any)
	if len(rows) != 2 {
		t.Fatalf("deselected inventory appeared in combined batch: %#v", rows)
	}
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["outcome"] != "reused" || row["path"] != "team/project" {
			t.Fatalf("account changed other selection: %#v", row)
		}
	}
	if _, err := os.Stat(filepath.Join(f.results, extraRef, "scan.json")); err != nil {
		t.Fatalf("account deselection discarded history: %v", err)
	}
	assertConnectionSecretsAbsent(t, f, "other-secret")
}

func TestDuplicateConnectionNamesAreRejectedBeforeExternalWork(t *testing.T) {
	f := newFixture(t)
	block := connectionBlock("duplicate", "gitlab", f.server.URL, "SWEEP_TEST_TOKEN", "team/project", "")
	connectionConfig(f, block+block)
	out, err := f.run()
	if err == nil || !strings.Contains(string(out), "connection names must be unique") {
		t.Fatalf("ambiguous connection accepted: %v %s", err, out)
	}
	if entries, _ := os.ReadDir(f.results); len(entries) != 0 {
		t.Fatalf("duplicate names mutated shared results: %v", entries)
	}
	if log, _ := os.ReadFile(filepath.Join(f.dir, "git.log")); len(log) != 0 {
		t.Fatalf("duplicate names began clone work: %s", log)
	}
}

func TestRuntimeConnectionFailuresDoNotDiscardOtherConnections(t *testing.T) {
	for _, failure := range []string{"authentication", "discovery", "clone", "scan"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			badEnv := "SWEEP_TEST_TOKEN"
			badPath := "team/broken"
			f.projects[badPath] = map[string]any{"id": 43, "path_with_namespace": badPath, "default_branch": "main", "http_url_to_repo": strings.Replace(f.server.URL, "http:", "https:", 1) + "/team/broken.git", "empty_repo": false}
			switch failure {
			case "authentication":
				badEnv = "SWEEP_BROKEN_TOKEN"
				t.Setenv(badEnv, "broken-secret")
				original := f.server.Config.Handler
				f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("PRIVATE-TOKEN") == "broken-secret" {
						http.Error(w, "broken-secret denied", 401)
						return
					}
					original.ServeHTTP(w, r)
				})
			case "discovery":
				delete(f.projects, badPath)
			case "clone":
				gitPath := filepath.Join(f.dir, "bin", "git")
				raw, _ := os.ReadFile(gitPath)
				raw = []byte(strings.Replace(string(raw), `if [ "$1" = clone ]; then`, `if [ "$1" = clone ]; then
  case "$*" in *broken.git*) exit 3 ;; esac`, 1))
				os.WriteFile(gitPath, raw, 0700)
			case "scan":
				gitPath := filepath.Join(f.dir, "bin", "git")
				raw, _ := os.ReadFile(gitPath)
				raw = []byte(strings.Replace(string(raw), `  exit $?`, `  status=$?
  case "$*" in *broken.git*) touch "$dest/.broken" ;; esac
  exit "$status"`, 1))
				os.WriteFile(gitPath, raw, 0700)
				os.WriteFile(f.scanner, []byte("#!/bin/sh\nfor arg in \"$@\"; do checkout=\"$arg\"; done\nprintf '%s\\n' '{\"schema_version\":1,\"root\":\"fixture\",\"sources\":[],\"check_runs\":[],\"findings\":[]}'\n[ ! -f \"$checkout/.broken\" ]\n"), 0700)
			}
			connectionConfig(f, connectionBlock("broken", "gitlab", f.server.URL, badEnv, badPath, "")+connectionBlock("healthy", "gitlab", f.server.URL, "SWEEP_TEST_TOKEN", "team/project", ""))
			out, err := f.run()
			if err == nil {
				t.Fatalf("runtime failure succeeded: %s", out)
			}
			summary := f.summary()
			counts := summary["counts"].(map[string]any)
			if counts["failed"] != float64(1) || counts["complete"] != float64(1) || summary["status"] != "incomplete" {
				t.Fatalf("one connection discarded another: %#v", summary)
			}
			if summary["discovery_complete"] != (failure != "authentication" && failure != "discovery") {
				t.Fatalf("wrong discovery completeness: %#v", summary)
			}
			healthyRef := ""
			for _, raw := range summary["repositories"].([]any) {
				row := raw.(map[string]any)
				if row["connection"] == "healthy" {
					if row["outcome"] != "complete" {
						t.Fatalf("healthy connection failed: %#v", row)
					}
					healthyRef = row["result"].(string)
				}
			}
			if healthyRef == "" {
				t.Fatal("healthy connection omitted")
			}
			if _, err := os.Stat(filepath.Join(f.results, healthyRef, "scan.json")); err != nil {
				t.Fatalf("healthy inventory discarded: %v", err)
			}
			if out, err = f.run(); err == nil {
				t.Fatalf("repeat runtime failure succeeded: %s", out)
			}
			counts = f.summary()["counts"].(map[string]any)
			if counts["failed"] != float64(1) || counts["reused"] != float64(1) {
				t.Fatalf("failed connection corrupted healthy reuse: %#v", counts)
			}
			f.assertNoSecretOrCheckout(out)
			assertConnectionSecretsAbsent(t, f, "broken-secret")
		})
	}
}

func connectionBlock(name, provider, server, env, paths, extra string) string {
	return fmt.Sprintf("  - name: %s\n    provider: %s\n    server_url: %s\n    token_env: %s\n    include:\n      repositories: [%s]\n%s", name, provider, server, env, paths, extra)
}

func TestMixedProviderConnectionsProduceIndependentInventoriesAndSharedScanInputs(t *testing.T) {
	f := newFixture(t)
	t.Setenv("SWEEP_SECOND_TOKEN", "other-secret")
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer other-secret" || r.Header.Get("PRIVATE-TOKEN") != "" {
			t.Error("GitHub received wrong connection credentials")
			http.Error(w, "unauthorized", 401)
			return
		}
		if strings.Contains(r.URL.Path, "/commits/") {
			json.NewEncoder(w).Encode(map[string]string{"sha": f.revision})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 42, "full_name": "team/project", "default_branch": "main", "clone_url": strings.Replace("http://"+r.Host, "http:", "https:", 1) + "/team/project.git"})
	}))
	t.Cleanup(second.Close)
	scannerRaw, _ := os.ReadFile(f.scanner)
	scannerRaw = append([]byte("#!/bin/sh\n[ -z \"$SWEEP_TEST_TOKEN\" ] && [ -z \"$SWEEP_SECOND_TOKEN\" ] || exit 99\n"), scannerRaw...)
	os.WriteFile(f.scanner, scannerRaw, 0700)
	gitPath := filepath.Join(f.dir, "bin", "git")
	raw, _ := os.ReadFile(gitPath)
	raw = []byte(strings.Replace(string(raw), `[ "$("$GIT_ASKPASS" 'Password for https://server')" = test-secret ] || exit 92`, `expected=test-secret
  case "$*" in *"$SWEEP_SECOND_SERVER"*) expected=other-secret ;; esac
  [ "$SWEEP_GIT_TOKEN" = "$expected" ] || exit 93
  [ "$("$GIT_ASKPASS" 'Password for https://server')" = "$expected" ] || exit 92`, 1))
	os.WriteFile(gitPath, raw, 0700)
	t.Setenv("SWEEP_SECOND_SERVER", strings.TrimPrefix(second.URL, "http://"))
	connections := connectionBlock("lab", "gitlab", f.server.URL, "SWEEP_TEST_TOKEN", "team/project, team/project", "") + connectionBlock("hub", "github", second.URL, "SWEEP_SECOND_TOKEN", "team/project", "")
	connectionConfig(f, connections)
	out, err := f.run()
	if err != nil {
		t.Fatalf("mixed batch: %v %s", err, out)
	}
	rows := f.summary()["repositories"].([]any)
	if len(rows) != 2 {
		t.Fatalf("connections were deduplicated or repositories duplicated: %#v", rows)
	}
	refs := map[string]string{}
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["outcome"] != "complete" || row["repository_id"] != "42" || row["path"] != "team/project" {
			t.Fatalf("mixed outcome: %#v", row)
		}
		name := row["connection"].(string)
		refs[name] = row["result"].(string)
		meta := reuseJSON(t, filepath.Join(f.results, refs[name], "metadata.json"))
		if meta["configuration"].(map[string]any)["disable_rules"].([]any)[0] != "shared-rule" {
			t.Fatalf("connection did not use shared scan inputs: %#v", meta)
		}
	}
	if refs["lab"] == refs["hub"] || !strings.HasPrefix(refs["lab"], "connections/lab/") || !strings.HasPrefix(refs["hub"], "connections/hub/") {
		t.Fatalf("history crossed connections: %#v", refs)
	}
	if out, err = f.run(); err != nil {
		t.Fatalf("repeat mixed batch: %v %s", err, out)
	}
	for _, raw := range f.summary()["repositories"].([]any) {
		row := raw.(map[string]any)
		if row["outcome"] != "reused" || row["result"] != refs[row["connection"].(string)] {
			t.Fatalf("connection reuse crossed histories: %#v", row)
		}
	}
	f.assertNoSecretOrCheckout(out)
	assertConnectionSecretsAbsent(t, f, "other-secret")
}

func assertConnectionSecretsAbsent(t *testing.T, f *fixture, secrets ...string) {
	t.Helper()
	filepath.Walk(f.results, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			raw, _ := os.ReadFile(path)
			for _, secret := range secrets {
				if strings.Contains(string(raw), secret) {
					t.Errorf("connection credential saved in %s", path)
				}
			}
		}
		return err
	})
}
