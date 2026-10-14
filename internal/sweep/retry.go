package sweep

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const maxAttempts = 3
const maxServerDelay = 30 * time.Second

// Fresh HTTP/1 connections prevent the transport from replaying a GET on a
// reused connection outside the explicit three-attempt budget.
func apiClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	// The clone retains the default transport's h2 ALPN offer. Limit that too,
	// so servers cannot negotiate a protocol this client no longer handles.
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = new(tls.Config)
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
}

func retryDelay(attempt int) time.Duration { return time.Duration(attempt) * 250 * time.Millisecond }

func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func temporaryTransportError(err error) bool {
	var network net.Error
	return errors.As(err, &network) && network.Timeout() || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE)
}

func temporaryAPIStatus(resp *http.Response, provider string) bool {
	switch resp.StatusCode {
	case 429, 500, 502, 503, 504:
		return true
	case 403:
		return provider == "GitHub" && (resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0")
	}
	return false
}

func serverRetryDelay(header string) (time.Duration, error) {
	if header == "" {
		return 0, nil
	}
	if seconds, err := strconv.ParseInt(header, 10, 64); err == nil {
		if seconds < 0 || seconds > int64(maxServerDelay/time.Second) {
			return 0, errors.New("server retry delay exceeds 30-second limit")
		}
		return time.Duration(seconds) * time.Second, nil
	}
	until, err := http.ParseTime(header)
	if err != nil {
		return 0, errors.New("invalid server retry delay")
	}
	delay := time.Until(until)
	if delay < 0 {
		delay = 0
	}
	if delay > maxServerDelay {
		return 0, errors.New("server retry delay exceeds 30-second limit")
	}
	return delay, nil
}

type apiStatusError struct {
	provider       string
	status         int
	attempts       int
	consecutive500 bool
}

func (e *apiStatusError) Error() string {
	return fmt.Sprintf("%s API retries exhausted after %d attempts (HTTP %d)", e.provider, e.attempts, e.status)
}

// Retry only this request. In particular, a failed pagination page does not
// restart discovery or replay pages that have already succeeded.
func apiDo(ctx context.Context, client *http.Client, req *http.Request, provider string, recursiveGroup ...bool) (*http.Response, error) {
	previous500 := false
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024+1))
			resp.Body.Close()
			if len(raw) > 16*1024*1024 {
				return nil, fmt.Errorf("%s API response exceeds 16 MiB", provider)
			}
			if readErr != nil {
				resp = nil
				err = readErr
			} else {
				resp.Body = io.NopCloser(bytes.NewReader(raw))
			}
		}
		if err != nil && (ctx.Err() != nil || !temporaryTransportError(err)) {
			return nil, fmt.Errorf("%s API request failed", provider)
		}
		if err == nil && !temporaryAPIStatus(resp, provider) {
			return resp, nil
		}
		current500 := err == nil && resp.StatusCode == http.StatusInternalServerError
		pair500 := len(recursiveGroup) > 0 && recursiveGroup[0] && previous500 && current500
		previous500 = current500
		if pair500 {
			resp.Body.Close()
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%s API retry canceled", provider)
			}
			requested, delayErr := serverRetryDelay(strings.TrimSpace(resp.Header.Get("Retry-After")))
			if delayErr != nil {
				return nil, fmt.Errorf("%s API: %s", provider, delayErr)
			}
			if deadline, ok := ctx.Deadline(); ok && requested > time.Until(deadline) {
				return nil, fmt.Errorf("%s API retry delay exceeds remaining timeout", provider)
			}
			return nil, &apiStatusError{provider: provider, status: resp.StatusCode, attempts: attempt, consecutive500: true}
		}
		if attempt == maxAttempts {
			if resp != nil {
				resp.Body.Close()
				return nil, &apiStatusError{provider: provider, status: resp.StatusCode, attempts: attempt}
			}
			return nil, fmt.Errorf("%s API retries exhausted after 3 attempts (transport failure)", provider)
		}
		delay := retryDelay(attempt)
		if resp != nil {
			requested, delayErr := serverRetryDelay(strings.TrimSpace(resp.Header.Get("Retry-After")))
			resp.Body.Close()
			if delayErr != nil {
				return nil, fmt.Errorf("%s API: %s", provider, delayErr)
			}
			if requested > delay {
				delay = requested
			}
			if deadline, ok := ctx.Deadline(); ok && requested > time.Until(deadline) {
				return nil, fmt.Errorf("%s API retry delay exceeds remaining timeout", provider)
			}
		}
		if err := waitRetry(ctx, delay); err != nil {
			return nil, fmt.Errorf("%s API retry canceled", provider)
		}
	}
	panic("unreachable retry attempt")
}

type gitOperationError struct{ temporary bool }

func (e *gitOperationError) Error() string {
	if e.temporary {
		return "Git operation failed (temporary network or server error)"
	}
	return "Git operation failed (permanent or unclassified error)"
}
func temporaryCloneError(stderr []byte) bool {
	message := strings.ToLower(string(stderr))
	for _, permanent := range []string{"authentication failed", "permission denied", "access denied", "not found", "repository does not exist", "could not read username", "certificate", "no space left on device", "disk quota exceeded", "returned error: 401", "returned error: 403"} {
		if strings.Contains(message, permanent) {
			return false
		}
	}
	for _, temporary := range []string{"could not resolve host", "connection timed out", "connection reset", "failed to connect", "remote end hung up unexpectedly", "early eof", "returned error: 429", "returned error: 500", "returned error: 502", "returned error: 503", "returned error: 504"} {
		if strings.Contains(message, temporary) {
			return true
		}
	}
	return false
}
