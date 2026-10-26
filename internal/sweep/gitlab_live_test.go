package sweep

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This opt-in test exercises production discovery without cloning an entire
// public group. It also checks membership against a separate recursive listing.
func TestLiveGitLabDiscovery(t *testing.T) {
	group := os.Getenv("SWEEP_LIVE_GITLAB_GROUP")
	if group == "" {
		t.Skip("set SWEEP_LIVE_GITLAB_GROUP and GITLAB_TOKEN to run live discovery")
	}
	token := os.Getenv("GITLAB_TOKEN")
	if token == "" {
		t.Fatal("GITLAB_TOKEN is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	c := Connection{Name: "live", Provider: "gitlab", ServerURL: "https://gitlab.com", token: token, Include: Selection{Groups: []string{group}}}
	var requests, listings, revisions atomic.Int64
	original := http.DefaultTransport
	transport := original.(*http.Transport).Clone()
	proxy := transport.Proxy
	// GitLab's free-plan burst limit can be 100 requests/minute. Pace only
	// this live test; production concurrency and retry behavior stay unchanged.
	var pacing sync.Mutex
	var nextRequest time.Time
	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		pacing.Lock()
		err := waitRetry(req.Context(), time.Until(nextRequest))
		nextRequest = time.Now().Add(750 * time.Millisecond)
		pacing.Unlock()
		if err != nil {
			return nil, err
		}
		requests.Add(1)
		if strings.Contains(req.URL.Path, "/repository/commits/") {
			revisions.Add(1)
		} else {
			listings.Add(1)
		}
		if proxy != nil {
			return proxy(req)
		}
		return nil, nil
	}
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = original }()
	started := time.Now()
	done := make(chan struct{})
	var progress sync.WaitGroup
	progress.Add(1)
	go func() {
		defer progress.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				t.Logf("elapsed=%s requests=%d listings=%d revisions=%d", time.Since(started).Round(time.Second), requests.Load(), listings.Load(), revisions.Load())
			case <-done:
				return
			}
		}
	}()
	var mu sync.Mutex
	var fallbacks []string
	var repos []Repository
	failures := discoverGitLab(ctx, c, func(group string) {
		mu.Lock()
		defer mu.Unlock()
		fallbacks = append(fallbacks, group)
		t.Logf("fallback group=%s", group)
	}, func(repo Repository) {
		mu.Lock()
		defer mu.Unlock()
		repos = append(repos, repo)
	})
	close(done)
	progress.Wait()
	elapsed := time.Since(started)
	report := map[string]any{
		"group": group, "started_at": started.UTC(), "elapsed_seconds": elapsed.Seconds(),
		"request_interval_ms": 750,
		"requests":            requests.Load(), "listing_requests": listings.Load(), "revision_requests": revisions.Load(),
		"fallbacks": fallbacks, "repositories": repos, "failures": failures,
	}
	t.Logf("discovery: repositories=%d failures=%d requests=%d listings=%d revisions=%d elapsed=%s", len(repos), len(failures), requests.Load(), listings.Load(), revisions.Load(), elapsed.Round(time.Second))
	// Compare IDs, not counts alone. Include repository resolution failures in
	// membership so a missing commit cannot hide a discovery omission.
	found := map[string]bool{}
	for _, repo := range repos {
		if found[repo.ID] {
			t.Errorf("duplicate repository ID %s", repo.ID)
		}
		found[repo.ID] = true
	}
	for _, failure := range failures {
		if failure.Repository.ID != "" {
			found[failure.Repository.ID] = true
		}
	}
	expected := map[string]bool{}
	err := gitlabList(ctx, c, "groups/"+url.PathEscape(group)+"/projects?include_subgroups=true&with_shared=false&per_page=100", func(p gitlabProject) {
		if !p.Archived && p.Fork == nil {
			expected[strconv.FormatInt(p.ID, 10)] = true
		}
	})
	report["reference_repositories"] = len(expected)
	if err != nil {
		t.Errorf("reference listing: %v", err)
	} else {
		for id := range expected {
			if !found[id] {
				t.Errorf("missing repository ID %s", id)
			}
		}
		for id := range found {
			if !expected[id] {
				t.Errorf("unexpected repository ID %s", id)
			}
		}
	}
	if path := os.Getenv("SWEEP_LIVE_GITLAB_REPORT"); path != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if len(failures) > 0 {
		t.Errorf("discovery returned %d failures; see the saved report", len(failures))
	}
	t.Logf("reference listing: %d selected repository IDs", len(expected))
}
