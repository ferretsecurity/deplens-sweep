package sweep_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTemporaryProviderFailuresRecoverDuringDiscoveryAndRevisionResolution(t *testing.T) {
	for _, provider := range []string{"gitlab", "github"} {
		t.Run(provider, func(t *testing.T) {
			var f *fixture
			if provider == "github" {
				f = newGitHubFixture(t)
			} else {
				f = newFixture(t)
			}
			original := f.server.Config.Handler
			var discovery, revision atomic.Int32
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				counter := &discovery
				if strings.Contains(r.URL.Path, "/commits/") {
					counter = &revision
				}
				if counter.Add(1) == 1 {
					w.Header().Set("Retry-After", "0")
					http.Error(w, "test-secret temporary failure", 503)
					return
				}
				original.ServeHTTP(w, r)
			})
			out, err := f.run()
			if err != nil {
				t.Fatalf("temporary failures not recovered: %v %s", err, out)
			}
			if discovery.Load() != 2 || revision.Load() != 2 {
				t.Fatalf("attempts discovery=%d revision=%d", discovery.Load(), revision.Load())
			}
			counts := f.summary()["counts"].(map[string]any)
			if counts["complete"] != float64(1) || counts["failed"] != float64(0) {
				t.Fatalf("counts=%#v", counts)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestTemporaryCloneFailureRetriesAfterRemovingIncompleteCheckout(t *testing.T) {
	f := newFixture(t)
	t.Setenv("SWEEP_RETRY_STATE", filepath.Join(f.dir, "clone-retries"))
	gitPath := filepath.Join(f.dir, "bin", "git")
	script, _ := os.ReadFile(gitPath)
	script = []byte(strings.Replace(string(script), "if [ \"$1\" = clone ]; then", `if [ "$1" = clone ]; then
  for arg in "$@"; do dest="$arg"; done
  if [ ! -f "$SWEEP_RETRY_STATE" ]; then
    mkdir -p "$dest"
    touch "$dest/incomplete"
    touch "$SWEEP_RETRY_STATE"
    echo 'fatal: unable to access test-secret: The requested URL returned error: 503' >&2
    exit 3
  fi
  [ ! -e "$dest" ] || exit 81`, 1))
	os.WriteFile(gitPath, script, 0700)
	out, err := f.run()
	if err != nil {
		t.Fatalf("temporary clone failure not recovered: %v %s", err, out)
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if strings.Count(string(log), "clone\n") != 2 {
		t.Fatalf("clone attempts: %s", log)
	}
	counts := f.summary()["counts"].(map[string]any)
	if counts["complete"] != float64(1) || counts["failed"] != float64(0) {
		t.Fatalf("counts=%#v", counts)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestProviderRetriesAreBoundedAndPermissionFailuresArePermanent(t *testing.T) {
	for _, provider := range []string{"gitlab", "github"} {
		for _, status := range []int{401, 403, 404, 429, 500, 502, 503, 504} {
			t.Run(fmt.Sprintf("%s-%d", provider, status), func(t *testing.T) {
				var f *fixture
				if provider == "github" {
					f = newGitHubFixture(t)
				} else {
					f = newFixture(t)
				}
				var requests atomic.Int32
				f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					http.Error(w, "test-secret rejected", status)
				})
				out, err := f.run()
				if err == nil {
					t.Fatalf("API failure accepted: %s", out)
				}
				expected := int32(1)
				if status >= 429 {
					expected = 3
				}
				if requests.Load() != expected {
					t.Fatalf("API requests=%d want=%d", requests.Load(), expected)
				}
				summary := f.summary()
				counts := summary["counts"].(map[string]any)
				if counts["failed"] != float64(1) || counts["empty"] != float64(0) || summary["discovery_complete"] != false {
					t.Fatalf("summary=%#v", summary)
				}
				if expected == 3 && !strings.Contains(string(out), "exhausted after 3 attempts") {
					t.Fatalf("exhaustion not visible: %s", out)
				}
				f.assertNoSecretOrCheckout(out)
			})
		}
	}
}

func TestGitHubRateLimit403NeedsPositiveEvidence(t *testing.T) {
	for _, header := range []string{"Retry-After", "X-RateLimit-Remaining"} {
		t.Run(header, func(t *testing.T) {
			f := newGitHubFixture(t)
			original := f.server.Config.Handler
			var requests atomic.Int32
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					w.Header().Set(header, "0")
					http.Error(w, "test-secret limited", 403)
					return
				}
				original.ServeHTTP(w, r)
			})
			out, err := f.run()
			if err != nil {
				t.Fatalf("rate limit not retried: %v %s", err, out)
			}
			if requests.Load() != 3 {
				t.Fatalf("rate-limited lookup not retried once: %d", requests.Load())
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestRetryAfterSecondsAndDateDelayTheNextRequest(t *testing.T) {
	for _, format := range []string{"seconds", "date"} {
		t.Run(format, func(t *testing.T) {
			f := newFixture(t)
			original := f.server.Config.Handler
			var requests atomic.Int32
			var earliest time.Time
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					if format == "seconds" {
						earliest = time.Now().Add(time.Second)
						w.Header().Set("Retry-After", "1")
					} else {
						earliest = time.Now().Add(2 * time.Second).Truncate(time.Second)
						w.Header().Set("Retry-After", earliest.UTC().Format(http.TimeFormat))
					}
					http.Error(w, "test-secret limited", 429)
					return
				}
				if time.Now().Before(earliest.Add(-20 * time.Millisecond)) {
					t.Errorf("retried before server delay elapsed")
				}
				original.ServeHTTP(w, r)
			})
			out, err := f.run()
			if err != nil {
				t.Fatalf("delayed retry: %v %s", err, out)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestExcessiveServerRetryDelayFailsWithoutRetryingEarly(t *testing.T) {
	f := newFixture(t)
	var requests atomic.Int32
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "31")
		http.Error(w, "test-secret limited", 429)
	})
	started := time.Now()
	out, err := f.run()
	if err == nil {
		t.Fatalf("excessive delay accepted: %s", out)
	}
	if requests.Load() != 1 || time.Since(started) > 2*time.Second {
		t.Fatalf("excessive delay retried or waited: requests=%d elapsed=%s", requests.Load(), time.Since(started))
	}
	if !strings.Contains(string(out), "30-second limit") {
		t.Fatalf("unclear bounded delay failure: %s", out)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestInterruptedAPIResponseRetriesTheSameOperation(t *testing.T) {
	f := newFixture(t)
	original := f.server.Config.Handler
	var requests atomic.Int32
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Length", "4096")
			fmt.Fprint(w, `{"message":"test-secret`)
			return
		}
		original.ServeHTTP(w, r)
	})
	out, err := f.run()
	if err != nil {
		t.Fatalf("interrupted response not recovered: %v %s", err, out)
	}
	if requests.Load() != 3 {
		t.Fatalf("interrupted lookup not retried once: %d", requests.Load())
	}
	f.assertNoSecretOrCheckout(out)
}

func TestCloneRetryBoundsAndPermanentFailures(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		attempts      int
	}{
		{"exhausted temporary", "fatal: The requested URL returned error: 503 test-secret", 3},
		{"authentication", "fatal: Authentication failed test-secret", 1},
		{"permission", "fatal: Permission denied test-secret; connection reset", 1},
		{"missing", "fatal: repository not found test-secret", 1},
		{"certificate", "fatal: SSL certificate problem test-secret", 1},
		{"disk", "fatal: No space left on device test-secret; early EOF", 1},
		{"unknown", "fatal: unexpected failure test-secret", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			t.Setenv("SWEEP_CLONE_FAILURE", tc.message)
			gitPath := filepath.Join(f.dir, "bin", "git")
			script, _ := os.ReadFile(gitPath)
			script = []byte(strings.Replace(string(script), "if [ \"$1\" = clone ]; then", `if [ "$1" = clone ]; then
  for arg in "$@"; do dest="$arg"; done
  [ ! -e "$dest/incomplete" ] || exit 81
  mkdir -p "$dest"
  touch "$dest/incomplete"
  echo "$SWEEP_CLONE_FAILURE" >&2
  exit 3`, 1))
			os.WriteFile(gitPath, script, 0700)
			out, err := f.run()
			if err == nil {
				t.Fatalf("clone failure accepted: %s", out)
			}
			log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
			if strings.Count(string(log), "clone\n") != tc.attempts {
				t.Fatalf("clone attempts=%d want=%d log=%s", strings.Count(string(log), "clone\n"), tc.attempts, log)
			}
			summary := f.summary()
			counts := summary["counts"].(map[string]any)
			if counts["failed"] != float64(1) || len(summary["repositories"].([]any)) != 1 {
				t.Fatalf("duplicated retry results: %#v", summary)
			}
			f.assertNoSecretOrCheckout(out)
		})
	}
}

func TestCloneRetryWaitUsesTheTotalRepositoryDeadline(t *testing.T) {
	f := newFixture(t)
	f.executionConfig("timeout: 150ms\n")
	gitPath := filepath.Join(f.dir, "bin", "git")
	script, _ := os.ReadFile(gitPath)
	script = []byte(strings.Replace(string(script), "if [ \"$1\" = clone ]; then", `if [ "$1" = clone ]; then
  for arg in "$@"; do dest="$arg"; done
  mkdir -p "$dest"
  echo 'fatal: connection reset test-secret' >&2
  exit 3`, 1))
	os.WriteFile(gitPath, script, 0700)
	started := time.Now()
	out, err := f.run()
	if err == nil {
		t.Fatalf("deadline accepted: %s", out)
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "git.log"))
	if strings.Count(string(log), "clone\n") != 1 {
		t.Fatalf("clone continued past budget: %s", log)
	}
	if !strings.Contains(string(out), "timeout") || time.Since(started) > 2*time.Second {
		t.Fatalf("retry delay exceeded total budget: %s", out)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestAnalysisFailureIsNotRetriedInsideTheBatch(t *testing.T) {
	f := newFixture(t)
	t.Setenv("SWEEP_SCAN_LOG", filepath.Join(f.dir, "scans.log"))
	scan, _ := os.ReadFile(f.scanner)
	scan = []byte(strings.Replace(string(scan), "#!/bin/sh\n", "#!/bin/sh\necho scan >> \"$SWEEP_SCAN_LOG\"\n", 1))
	scan = append(scan, []byte("exit 3\n")...)
	os.WriteFile(f.scanner, scan, 0700)
	out, err := f.run()
	if err == nil {
		t.Fatalf("analysis failure accepted: %s", out)
	}
	log, _ := os.ReadFile(os.Getenv("SWEEP_SCAN_LOG"))
	if strings.Count(string(log), "scan\n") != 1 {
		t.Fatalf("analysis retried: %s", log)
	}
	f.assertNoSecretOrCheckout(out)
}

func TestGitDoesNotInheritTraceOrInjectedConfiguration(t *testing.T) {
	f := newFixture(t)
	t.Setenv("GIT_TRACE", "test-secret")
	t.Setenv("GIT_CURL_VERBOSE", "test-secret")
	t.Setenv("GIT_CONFIG_PARAMETERS", "test-secret")
	gitPath := filepath.Join(f.dir, "bin", "git")
	script, _ := os.ReadFile(gitPath)
	script = []byte(strings.Replace(string(script), "#!/bin/sh\n", `#!/bin/sh
[ -z "$GIT_TRACE" ] && [ -z "$GIT_CURL_VERBOSE" ] && [ -z "$GIT_CONFIG_PARAMETERS" ] || exit 81
[ "$LC_ALL" = C ] || exit 82
`, 1))
	os.WriteFile(gitPath, script, 0700)
	out, err := f.run()
	if err != nil {
		t.Fatalf("Git inherited unsafe diagnostics: %v %s", err, out)
	}
	f.assertNoSecretOrCheckout(out)
}
