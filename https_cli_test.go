package sweep_test

import (
	"crypto/tls"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHTTPSProvidersAdvertisingHTTP2ProduceCompleteInventories(t *testing.T) {
	for _, provider := range []string{"github", "gitlab"} {
		t.Run(provider, func(t *testing.T) {
			var f *fixture
			if provider == "github" {
				f = newGitHubFixture(t)
			} else {
				f = newFixture(t)
			}
			original := f.getHandler()
			f.server.Close()
			f.server = httptest.NewUnstartedServer(f.wrapHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 1 {
					t.Errorf("provider request used %s instead of HTTP/1.1", r.Proto)
				}
				original.ServeHTTP(w, r)
			})))
			f.server.EnableHTTP2 = true
			f.server.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
			f.server.StartTLS()
			t.Cleanup(f.server.Close)
			caPath := filepath.Join(f.dir, "provider-ca.pem")
			certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw})
			if err := os.WriteFile(caPath, certificate, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SSL_CERT_FILE", caPath)
			t.Setenv("NO_PROXY", "*")
			cloneKey := "http_url_to_repo"
			if provider == "github" {
				cloneKey = "clone_url"
			}
			f.projects["team/project"][cloneKey] = f.server.URL + "/team/project.git"
			if provider == "github" {
				writeGitHubConfig(f, "  - team/project\n", "")
			} else {
				f.writeConfig("  - team/project\n", "")
			}
			out, err := f.run()
			if err != nil {
				t.Fatalf("HTTPS provider scan failed: %v %s", err, out)
			}
			summary := f.summary()
			if summary["discovery_complete"] != true || summary["counts"].(map[string]any)["complete"] != float64(1) {
				t.Fatalf("HTTPS provider inventory incomplete: %#v", summary)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}
