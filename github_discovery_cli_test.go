package sweep_test

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"time"

	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func githubRepositoryFixture(f *fixture, id int, path string) map[string]any {
	return map[string]any{"id": id, "full_name": path, "default_branch": "main", "clone_url": strings.Replace(f.server.URL, "http:", "https:", 1) + "/" + path + ".git"}
}

func githubDiscoveryServer(f *fixture, list http.HandlerFunc) {
	f.t.Helper()
	f.server.Close()
	f.server = f.startServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			f.t.Error("missing GitHub API authentication")
			http.Error(w, "test-secret denied", 401)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/api/v3/repos/")
		if path, _, commit := strings.Cut(p, "/commits/"); commit {
			if _, exists := f.projects[path]; !exists {
				f.t.Errorf("unexpected commit resolution: %s", path)
				http.Error(w, "missing repository", 404)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"sha": f.revision})
			return
		}
		if repo, exists := f.projects[p]; exists {
			json.NewEncoder(w).Encode(repo)
			return
		}
		list(w, r)
	}))
	f.t.Cleanup(f.server.Close)
	for _, repo := range f.projects {
		repo["clone_url"] = strings.Replace(f.server.URL, "http:", "https:", 1) + "/" + repo["full_name"].(string) + ".git"
	}
}

func TestGitHubOrganizationDiscoversRepositories(t *testing.T) {
	f := newGitHubFixture(t)
	f.projects["team/library"] = githubRepositoryFixture(f, 43, "team/library")
	githubDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/orgs/team/repos" {
			t.Errorf("organization API: %s", r.URL)
			http.Error(w, "unexpected API", 404)
			return
		}
		json.NewEncoder(w).Encode([]any{f.projects["team/project"], f.projects["team/library"]})
	})
	writeGitHubConfig(f, "", "      organizations: [team]\n")
	out, err := f.run()
	if err != nil {
		t.Fatalf("organization scan: %v %s", err, out)
	}
	results := f.summary()["repositories"].([]any)
	if len(results) != 2 {
		t.Fatalf("organization repositories: %#v", results)
	}
	for _, result := range results {
		item := result.(map[string]any)
		if item["outcome"] != "complete" || item["provider"] != "github" {
			t.Fatalf("outcome: %#v", item)
		}
		if _, err := os.Stat(filepath.Join(f.results, item["result"].(string), "scan.json")); err != nil {
			t.Fatal(err)
		}
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitHubOrganizationExclusionsAndDefaults(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "enabled"}[enabled], func(t *testing.T) {
			f := newGitHubFixture(t)
			for i, path := range []string{"team/excluded", "team/archived", "team/fork", "excluded/project", "excluded-other/project"} {
				f.projects[path] = githubRepositoryFixture(f, 43+i, path)
			}
			f.projects["old/path"] = f.projects["excluded/project"]
			f.projects["team/archived"]["archived"] = true
			f.projects["team/fork"]["fork"] = true
			githubDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v3/orgs/team/repos":
					json.NewEncoder(w).Encode([]any{f.projects["team/project"], f.projects["team/excluded"], f.projects["team/archived"], f.projects["team/fork"]})
				case "/api/v3/orgs/excluded-other/repos":
					json.NewEncoder(w).Encode([]any{f.projects["excluded-other/project"]})
				default:
					t.Errorf("excluded organization or unexpected request: %s", r.URL)
					http.Error(w, "test-secret unavailable", 403)
				}
			})
			extra := "      organizations: [team, team, excluded, excluded-other]\n    exclude:\n      repositories: [team/excluded]\n      organizations: [excluded]\n"
			if enabled {
				extra += "    include_archived: true\n    include_forks: true\n"
			}
			writeGitHubConfig(f, "  - team/project\n  - excluded/project\n  - old/path\n", extra)
			out, err := f.run()
			if err != nil {
				t.Fatalf("organization scan: %v %s", err, out)
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
				path := result.(map[string]any)["path"]
				if path == "team/excluded" || path == "excluded/project" {
					t.Fatalf("excluded repository selected: %s", path)
				}
			}
			log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
			if strings.Count(string(log), "clone\n") != want {
				t.Fatalf("deduplication: %s", log)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitHubOrganizationPaginationFindsNewRepositories(t *testing.T) {
	f := newGitHubFixture(t)
	f.projects["team/library"] = githubRepositoryFixture(f, 43, "team/library")
	f.projects["team/new-project"] = githubRepositoryFixture(f, 44, "team/new-project")
	added := false
	githubDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/orgs/team/repos" {
			t.Errorf("organization request: %s", r.URL)
			http.Error(w, "unexpected API", 404)
			return
		}
		if r.URL.Query().Get("page") != "2" {
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/v3/orgs/team/repos?page=2&per_page=100>; rel="next", <%s/api/v3/orgs/team/repos?page=2&per_page=100>; rel="last"`, f.server.URL, f.server.URL))
			json.NewEncoder(w).Encode([]any{f.projects["team/project"]})
			return
		}
		repositories := []any{f.projects["team/library"], f.projects["team/project"]}
		if added {
			repositories = append(repositories, f.projects["team/new-project"])
		}
		json.NewEncoder(w).Encode(repositories)
	})
	writeGitHubConfig(f, "  - team/project\n", "      organizations: [team]\n")
	out, err := f.run()
	if err != nil {
		t.Fatalf("first scan: %v %s", err, out)
	}
	if len(f.summary()["repositories"].([]any)) != 2 {
		t.Fatalf("second page omitted: %#v", f.summary())
	}
	added = true
	out, err = f.run()
	if err != nil {
		t.Fatalf("later batch: %v %s", err, out)
	}
	summary := f.summary()
	counts := summary["counts"].(map[string]any)
	if len(summary["repositories"].([]any)) != 3 || counts["complete"] != float64(1) || counts["reused"] != float64(2) {
		t.Fatalf("new repository discovery: %#v", summary)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitHubAllAccessibleRequiresOptInAndHonorsFilters(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint("archive-and-forks-", enabled), func(t *testing.T) {
			f := newGitHubFixture(t)
			for i, path := range []string{"team/archived", "team/fork", "excluded/project", "team/excluded", "excluded-other/project"} {
				f.projects[path] = githubRepositoryFixture(f, 43+i, path)
			}
			f.projects["team/archived"]["archived"] = true
			f.projects["team/fork"]["fork"] = true
			githubDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v3/user/repos" {
					t.Errorf("all-accessible API: %s", r.URL)
					http.Error(w, "unexpected API", 404)
					return
				}
				if r.URL.Query().Get("visibility") != "all" || r.URL.Query().Get("affiliation") != "owner,collaborator,organization_member" {
					t.Errorf("all-accessible scope: %s", r.URL)
				}
				if r.URL.Query().Get("page") == "2" {
					json.NewEncoder(w).Encode([]any{f.projects["excluded-other/project"], f.projects["team/project"]})
					return
				}
				w.Header().Set("Link", fmt.Sprintf(`<%s/api/v3/user/repos?page=2&visibility=all&affiliation=owner,collaborator,organization_member>; rel="next"`, f.server.URL))
				json.NewEncoder(w).Encode([]any{f.projects["team/project"], f.projects["team/archived"], f.projects["team/fork"], f.projects["excluded/project"], f.projects["team/excluded"]})
			})
			writeGitHubConfig(f, "", "    all_accessible: false\n")
			out, err := f.run()
			if err == nil || !strings.Contains(string(out), "explicit repository selection") {
				t.Fatalf("implicit all-accessible accepted: %v %s", err, out)
			}
			extra := "    all_accessible: true\n    exclude:\n      repositories: [team/excluded]\n      organizations: [excluded]\n"
			if enabled {
				extra += "    include_archived: true\n    include_forks: true\n"
			}
			writeGitHubConfig(f, "", extra)
			out, err = f.run()
			if err != nil {
				t.Fatalf("all-accessible scan: %v %s", err, out)
			}
			summary := f.summary()
			results := summary["repositories"].([]any)
			want := 2
			if enabled {
				want = 4
			}
			if len(results) != want {
				t.Fatalf("selected repositories: %#v", results)
			}
			for _, result := range results {
				path := result.(map[string]any)["path"]
				if path == "team/excluded" || path == "excluded/project" {
					t.Fatalf("excluded project scanned: %s", path)
				}
			}
			selection := summary["selections"].([]any)[0].(map[string]any)
			if selection["all_accessible"] != true {
				t.Fatalf("all-accessible missing from batch: %#v", selection)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitHubIncompleteDiscoveryStillScansKnownRepositories(t *testing.T) {
	f := newGitHubFixture(t)
	f.projects["personal/project"] = githubRepositoryFixture(f, 43, "personal/project")
	githubDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/orgs/team/repos":
			if r.URL.Query().Get("page") == "2" {
				http.Error(w, "test-secret server error", 500)
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/v3/orgs/team/repos?page=2>; rel="next"`, f.server.URL))
			json.NewEncoder(w).Encode([]any{f.projects["team/project"]})
		case "/api/v3/orgs/denied/repos":
			http.Error(w, "test-secret forbidden", 403)
		case "/api/v3/user/repos":
			http.Error(w, "test-secret unauthorized", 401)
		default:
			t.Errorf("unexpected API: %s", r.URL)
			http.Error(w, "unexpected", 404)
		}
	})
	writeGitHubConfig(f, "  - personal/project\n", "      organizations: [team, denied]\n    all_accessible: true\n")
	out, err := f.run()
	if err == nil {
		t.Fatalf("incomplete discovery accepted: %s", out)
	}
	summary := f.summary()
	counts := summary["counts"].(map[string]any)
	if summary["discovery_complete"] != false || counts["failed"] != float64(3) || counts["complete"] != float64(2) {
		t.Fatalf("known repositories or failures lost: %#v", summary)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitHubPaginationRejectsUntrustedURLsAndLoops(t *testing.T) {
	for _, mode := range []string{"other-server", "credentials", "fragment", "outside-api", "loop", "empty-next", "invalid-array"} {
		t.Run(mode, func(t *testing.T) {
			f := newGitHubFixture(t)
			destination := httptest.NewServer(f.wrapHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("contacted untrusted pagination server")
				if r.Header.Get("Authorization") != "" {
					t.Error("token forwarded")
				}
			})))
			t.Cleanup(destination.Close)
			githubDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
				next := f.server.URL + r.URL.RequestURI()
				switch mode {
				case "other-server":
					next = destination.URL + "/api/v3/orgs/team/repos"
				case "credentials":
					next = strings.Replace(next, "://", "://user:test-secret@", 1)
				case "fragment":
					next += "#test-secret"
				case "outside-api":
					next = f.server.URL + "/elsewhere"
				case "empty-next":
					next = ""
				case "invalid-array":
					fmt.Fprint(w, "null")
					return
				}
				w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
				json.NewEncoder(w).Encode([]any{f.projects["team/project"]})
			})
			writeGitHubConfig(f, "  - team/project\n", "      organizations: [team]\n")
			out, err := f.run()
			if err == nil {
				t.Fatalf("invalid discovery accepted: %s", out)
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

func TestGitHubEnterpriseOrganizationPaginationWithInstallationPath(t *testing.T) {
	f := newGitHubFixture(t)
	f.server.Close()
	f.server = f.startServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("missing API authentication")
		}
		switch r.URL.Path {
		case "/installation/api/v3/orgs/team/repos":
			if r.URL.Query().Get("page") == "2" {
				json.NewEncoder(w).Encode([]any{f.projects["team/library"]})
				return
			}
			w.Header().Set("Link", `<?page=2>; rel="next"`)
			json.NewEncoder(w).Encode([]any{f.projects["team/project"]})
		case "/installation/api/v3/repos/team/project/commits/main", "/installation/api/v3/repos/team/library/commits/main":
			json.NewEncoder(w).Encode(map[string]string{"sha": f.revision})
		default:
			t.Errorf("enterprise API request: %s", r.URL)
			http.Error(w, "unexpected API", 404)
		}
	}))
	t.Cleanup(f.server.Close)
	f.projects["team/library"] = githubRepositoryFixture(f, 43, "team/library")
	for _, repo := range f.projects {
		repo["clone_url"] = strings.Replace(f.server.URL, "http:", "https:", 1) + "/installation/" + repo["full_name"].(string) + ".git"
	}
	writeGitHubConfig(f, "", "      organizations: [team]\n")
	data, _ := os.ReadFile(f.config)
	os.WriteFile(f.config, []byte(strings.ReplaceAll(string(data), f.server.URL, f.server.URL+"/installation")), 0600)
	out, err := f.run()
	if err != nil {
		t.Fatalf("enterprise organization scan: %v %s", err, out)
	}
	results := f.summary()["repositories"].([]any)
	if len(results) != 2 {
		t.Fatalf("enterprise pagination lost a repository: %#v", results)
	}
	for _, result := range results {
		if result.(map[string]any)["server_url"] != f.server.URL+"/installation" {
			t.Fatalf("enterprise identity: %#v", result)
		}
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitHubOrganizationSelectionsRejectWildcardsAndNestedNames(t *testing.T) {
	cases := []struct {
		name, extra string
		gitlab      bool
	}{
		{name: "wildcard", extra: "      organizations: ['team*']\n"},
		{name: "wildcard exclusion", extra: "    exclude:\n      organizations: ['team*']\n"},
		{name: "nested organization", extra: "      organizations: [team/subgroup]\n"},
		{name: "gitlab organization", extra: "      organizations: [team]\n", gitlab: true},
		{name: "gitlab organization exclusion", extra: "    exclude:\n      organizations: [team]\n", gitlab: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGitHubFixture(t)
			writeGitHubConfig(f, "  - team/project\n", tc.extra)
			if tc.gitlab {
				data, _ := os.ReadFile(f.config)
				os.WriteFile(f.config, []byte(strings.Replace(string(data), "provider: github", "provider: gitlab", 1)), 0600)
			}
			out, err := f.run()
			if err == nil {
				t.Fatalf("invalid selection accepted: %s", out)
			}
			if _, err := os.Stat(f.results); !os.IsNotExist(err) {
				t.Fatalf("invalid selection created batch state: %v", err)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func publicGitHubProxy(f *fixture, respond func(*http.Request) (any, string)) {
	t := f.t
	t.Helper()
	// The CLI connects through a local TLS proxy so the real public API hostname
	// and certificate checks are exercised without a live GitHub account.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "api.github.com"}, DNSNames: []string{"api.github.com"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(f.dir, "ca.pem")
	if err = os.WriteFile(caPath, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(f.wrapHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "api.github.com:443" {
			t.Errorf("unexpected proxy target: %s %s", r.Method, r.Host)
			http.Error(w, "unexpected target", 400)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{pair}})
		request, err := http.ReadRequest(bufio.NewReader(secure))
		if err != nil {
			t.Error(err)
			return
		}
		if request.Host != "api.github.com" || request.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("public API request: %s, token header correct: %v", request.Host, request.Header.Get("Authorization") == "Bearer test-secret")
		}
		payload, link := respond(request)
		body, _ := json.Marshal(payload)
		header := ""
		if link != "" {
			header = "Link: " + link + "\r\n"
		}
		fmt.Fprintf(secure, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n%s\r\n%s", len(body), header, body)
	})))
	t.Cleanup(proxy.Close)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("SSL_CERT_FILE", caPath)
}

func TestGitHubPublicOrganizationAndAllAccessiblePagination(t *testing.T) {
	for _, mode := range []string{"organization", "all-accessible"} {
		t.Run(mode, func(t *testing.T) {
			f := newGitHubFixture(t)
			f.projects["team/library"] = githubRepositoryFixture(f, 43, "team/library")
			for _, repo := range f.projects {
				repo["clone_url"] = "https://github.com/" + repo["full_name"].(string) + ".git"
			}
			endpoint := "/orgs/team/repos"
			extra := "      organizations: [team]\n"
			if mode == "all-accessible" {
				endpoint = "/user/repos"
				extra = "    all_accessible: true\n"
			}
			publicGitHubProxy(f, func(request *http.Request) (any, string) {
				if request.URL.Path == endpoint {
					if request.URL.Query().Get("page") == "2" {
						return []any{f.projects["team/library"]}, ""
					}
					return []any{f.projects["team/project"]}, fmt.Sprintf(`<https://api.github.com%s?page=2>; rel="next"`, endpoint)
				}
				switch request.URL.Path {
				case "/repos/team/project/commits/main", "/repos/team/library/commits/main":
					return map[string]string{"sha": f.revision}, ""
				default:
					t.Errorf("unexpected public API: %s", request.URL)
					return map[string]string{}, ""
				}
			})
			writeGitHubConfig(f, "", extra)
			data, _ := os.ReadFile(f.config)
			os.WriteFile(f.config, []byte(strings.ReplaceAll(string(data), f.server.URL, "https://github.com")), 0600)
			out, err := f.run()
			if err != nil {
				t.Fatalf("public discovery scan: %v %s", err, out)
			}
			results := f.summary()["repositories"].([]any)
			if len(results) != 2 {
				t.Fatalf("public pagination lost repository: %#v", results)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestGitHubOrganizationRetriesOnlyTheFailedPage(t *testing.T) {
	f := newGitHubFixture(t)
	f.projects["team/library"] = githubRepositoryFixture(f, 43, "team/library")
	var firstPage, secondPage atomic.Int32
	githubDiscoveryServer(f, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "2" {
			firstPage.Add(1)
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/v3/orgs/team/repos?page=2>; rel="next"`, f.server.URL))
			json.NewEncoder(w).Encode([]any{f.projects["team/project"]})
			return
		}
		if secondPage.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "test-secret unavailable", 503)
			return
		}
		json.NewEncoder(w).Encode([]any{f.projects["team/library"]})
	})
	writeGitHubConfig(f, "", "      organizations: [team]\n")
	out, err := f.run()
	if err != nil {
		t.Fatalf("recovered page failure: %v %s", err, out)
	}
	if firstPage.Load() != 1 || secondPage.Load() != 2 {
		t.Fatalf("discovery replayed earlier page: first=%d second=%d", firstPage.Load(), secondPage.Load())
	}
	summary := f.summary()
	if summary["discovery_complete"] != true || summary["counts"].(map[string]any)["complete"] != float64(2) {
		t.Fatalf("recovered discovery: %#v", summary)
	}
	f.assertNoSecretOrCheckout(out)
}
