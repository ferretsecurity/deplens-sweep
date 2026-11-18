package sweep

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type retryTransport func(*http.Request) (*http.Response, error)

func (f retryTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestAPIRetryAfterSecondsAndDates(t *testing.T) {
	for _, format := range []string{"seconds", "date"} {
		for _, seconds := range []int{31, 60, 120, 121} {
			t.Run(format+"/"+strconv.Itoa(seconds), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					delay := time.Duration(seconds) * time.Second
					header := strconv.Itoa(seconds)
					if format == "date" {
						header = time.Now().Add(delay).UTC().Format(http.TimeFormat)
					}
					calls := 0
					started := time.Now()
					client := &http.Client{Transport: retryTransport(func(req *http.Request) (*http.Response, error) {
						calls++
						status := http.StatusOK
						if calls == 1 {
							status = http.StatusTooManyRequests
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{header}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
					})}
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://gitlab.example/api/v4/projects", nil)
					if err != nil {
						t.Fatal(err)
					}
					resp, err := apiDo(t.Context(), client, req, "GitLab")
					if seconds == 121 {
						if err == nil || !strings.Contains(err.Error(), "120-second limit") || calls != 1 || time.Since(started) != 0 {
							t.Fatalf("excessive delay: calls=%d elapsed=%s error=%v", calls, time.Since(started), err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					if calls != 2 || time.Since(started) != delay {
						t.Fatalf("calls=%d elapsed=%s, want 2 calls after %s", calls, time.Since(started), delay)
					}
				})
			})
		}
	}
}

func TestInvalidServerRetryAfter(t *testing.T) {
	for _, header := range []string{"-1", "malformed"} {
		t.Run(header, func(t *testing.T) {
			if _, err := serverRetryDelay(header); err == nil {
				t.Fatalf("invalid Retry-After %q accepted", header)
			}
		})
	}
}

func TestAPIRetryAfterRespectsDeadlineAndCancellation(t *testing.T) {
	for _, cancelDuringWait := range []bool{false, true} {
		name := "deadline"
		if cancelDuringWait {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				if cancelDuringWait {
					cancel()
					ctx, cancel = context.WithCancel(t.Context())
					timer := time.AfterFunc(time.Second, cancel)
					defer timer.Stop()
				}
				defer cancel()
				calls := 0
				client := &http.Client{Transport: retryTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
				})}
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://gitlab.example/api/v4/projects", nil)
				if err != nil {
					t.Fatal(err)
				}
				started := time.Now()
				_, err = apiDo(ctx, client, req, "GitLab")
				wantError := "retry delay exceeds remaining timeout"
				wantElapsed := time.Duration(0)
				if cancelDuringWait {
					wantError = "retry canceled"
					wantElapsed = time.Second
				}
				if err == nil || !strings.Contains(err.Error(), wantError) || calls != 1 || time.Since(started) != wantElapsed {
					t.Fatalf("calls=%d elapsed=%s error=%v, want %s after %s", calls, time.Since(started), err, wantError, wantElapsed)
				}
			})
		})
	}
}
