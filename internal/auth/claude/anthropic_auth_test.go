package claude

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshTokens_UsesIndependentTimeout(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	callerCtx, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	var requestDeadline time.Time
	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var ok bool
				requestDeadline, ok = req.Context().Deadline()
				if !ok {
					t.Fatal("refresh request has no deadline")
				}
				if errContext := req.Context().Err(); errContext != nil {
					t.Fatalf("refresh request context is already done: %v", errContext)
				}
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Body:       io.NopCloser(strings.NewReader(`{"error":"probe"}`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}

	_, err := auth.RefreshTokens(callerCtx, "independent-timeout-token")
	if err == nil {
		t.Fatal("expected refresh error")
	}
	if requestDeadline.IsZero() || !requestDeadline.After(time.Now()) {
		t.Fatalf("refresh deadline = %v, want a future deadline", requestDeadline)
	}
}

// jsonResponse builds a canned control-plane response for the fake transport.
func jsonResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}
}

type errorReader struct {
	err error
}

func (r *errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

func (r *errorReader) Close() error {
	return nil
}

func TestRefreshTokensWithRetry_DoesNotReplayAfterResponseReadError(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       &errorReader{err: io.ErrUnexpectedEOF},
					Header:     make(http.Header),
				}, nil
			}),
		},
	}

	_, err := auth.RefreshTokensWithRetry(context.Background(), "single-use-refresh-token", 3)
	if err == nil {
		t.Fatal("expected refresh error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected one refresh attempt after a response decode/read error, got %d", got)
	}
}

func TestRefreshTokensWithRetry_DoesNotReplayAfterJSONDecodeError(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`invalid json payload`)),
					Header:     make(http.Header),
				}, nil
			}),
		},
	}

	_, err := auth.RefreshTokensWithRetry(context.Background(), "single-use-refresh-token", 3)
	if err == nil {
		t.Fatal("expected refresh error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected one refresh attempt after a JSON decode error, got %d", got)
	}
}

func TestRefreshTokensWithRetry_DoesNotReplayAfterTransportError(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return nil, errors.New("connection reset by peer")
			}),
		},
	}

	_, err := auth.RefreshTokensWithRetry(context.Background(), "single-use-refresh-token", 3)
	if err == nil {
		t.Fatal("expected refresh error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected one refresh attempt after an ambiguous transport error, got %d", got)
	}
}

func TestRefreshTokensWithRetry_429BlocksImmediateReplay(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`{"error":"rate_limited"}`)),
					Header:     http.Header{"Retry-After": []string{"60"}},
					Request:    req,
				}, nil
			}),
		},
	}

	_, err := auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
	if err == nil {
		t.Fatalf("expected 429 refresh error")
	}
	if !strings.Contains(err.Error(), "status 429") {
		t.Fatalf("expected status 429 in error, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 refresh attempt after 429, got %d", got)
	}

	_, err = auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
	if err == nil {
		t.Fatalf("expected immediate blocked refresh error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected blocked retry to avoid a second refresh call, got %d attempts", got)
	}
	if blockedUntil := claudeRefreshBlockedUntil("dummy_refresh_token"); !blockedUntil.After(time.Now()) {
		t.Fatalf("expected blocked-until timestamp to be set, got %v", blockedUntil)
	}
}

func TestRefreshTokens_DeduplicatesConcurrentRefresh(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var tokenCalls int32

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.String() {
				case RefreshTokenURL:
					atomic.AddInt32(&tokenCalls, 1)
					once.Do(func() { close(started) })
					<-release
					return &http.Response{
						StatusCode: http.StatusOK,
						Body: io.NopCloser(strings.NewReader(`{
							"access_token":"new-access",
							"refresh_token":"new-refresh",
							"token_type":"Bearer",
 "account":{"uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"},"organization":{"uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","name":"Shared Org"},
 "expires_in":3600,
							"scope":"user:profile user:inference"
						}`)),
						Header:  make(http.Header),
						Request: req,
					}, nil
				default:
					t.Fatalf("unexpected OAuth request URL %s", req.URL)
					return nil, nil
				}
			}),
		},
	}

	results := make(chan *ClaudeTokenData, 2)
	errs := make(chan error, 2)
	runRefresh := func() {
		td, err := auth.RefreshTokens(context.Background(), "shared-refresh-token")
		results <- td
		errs <- err
	}

	go runRefresh()
	go runRefresh()

	<-started
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Fatalf("expected concurrent refresh to share a single upstream call, got %d", got)
	}
	close(release)

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("expected refresh to succeed, got %v", err)
		}
		td := <-results
		if td == nil || td.AccessToken != "new-access" {
			t.Fatalf("expected refreshed access token, got %#v", td)
		}
		if td.AccountUUID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
			t.Fatalf("account UUID = %q, want OAuth response account", td.AccountUUID)
		}
		if td.OrganizationUUID != "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" || td.OrganizationName != "Shared Org" {
			t.Fatalf("organization = %q/%q, want OAuth response organization", td.OrganizationUUID, td.OrganizationName)
		}
	}
	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Fatalf("expected exactly 1 upstream refresh call, got %d", got)
	}
}

func TestUpdateTokenStoragePreservesAccountWhenRefreshOmitsIt(t *testing.T) {
	storage := &ClaudeTokenStorage{
		Email:            "user@example.com",
		AccountUUID:      "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		OrganizationUUID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		OrganizationName: "Example Org",
	}
	(&ClaudeAuth{}).UpdateTokenStorage(storage, &ClaudeTokenData{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		Expire:       "2099-01-01T00:00:00Z",
	})

	if storage.Email != "user@example.com" {
		t.Fatalf("email = %q, want preserved", storage.Email)
	}
	if storage.AccountUUID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
		t.Fatalf("account UUID = %q, want preserved", storage.AccountUUID)
	}
	if storage.OrganizationUUID != "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" || storage.OrganizationName != "Example Org" {
		t.Fatalf("organization = %q/%q, want preserved", storage.OrganizationUUID, storage.OrganizationName)
	}
}
