package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type retryAfterProvider interface {
	RetryAfter() *time.Duration
}

func TestClaudeExecutor_HonorsAnthropicRateLimitHeaders_Execute(t *testing.T) {
	now := time.Now()
	sevenDayReset := now.Add(7 * 24 * time.Hour).Unix()
	fiveHourReset := now.Add(5 * time.Hour).Unix()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(fiveHourReset, 10))
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(sevenDayReset, 10))
		w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "seven_day")
		w.Header().Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(sevenDayReset, 10))
		w.Header().Set("Retry-After", strconv.FormatInt(7*24*3600, 10))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your 7-day rate limit."}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "claude-auth-1",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		},
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err == nil {
		t.Fatal("expected error from Execute, got nil")
	}

	var rap retryAfterProvider
	if !errors.As(err, &rap) || rap == nil {
		t.Fatalf("expected error %T to implement RetryAfter() *time.Duration", err)
	}

	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		t.Fatalf("expected non-nil RetryAfter, got nil")
	}

	// Should be at least 7 days (reported reset) and at most 7 days + 35s (fuzz upper bound).
	minExpected := 7*24*time.Hour - 5*time.Second
	maxExpected := 7*24*time.Hour + 35*time.Second
	if *retryAfter < minExpected || *retryAfter > maxExpected {
		t.Fatalf("RetryAfter = %v, want between %v and %v", *retryAfter, minExpected, maxExpected)
	}

	// Verify one-time fuzz stability: repeat calls return exact same value
	if second := rap.RetryAfter(); second == nil || *second != *retryAfter {
		t.Fatalf("RetryAfter changed across calls: %v vs %v", *second, *retryAfter)
	}
}

func TestClaudeExecutor_HonorsAnthropicRateLimitHeaders_ExecuteStream(t *testing.T) {
	now := time.Now()
	fiveHourReset := now.Add(5 * time.Hour).Unix()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(fiveHourReset, 10))
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(now.Add(7*24*time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"5-hour limit exceeded."}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "claude-auth-1",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		},
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	_, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err == nil {
		t.Fatal("expected error from ExecuteStream, got nil")
	}

	var rap retryAfterProvider
	if !errors.As(err, &rap) || rap == nil {
		t.Fatalf("expected error %T to implement RetryAfter() *time.Duration", err)
	}

	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		t.Fatalf("expected non-nil RetryAfter, got nil")
	}

	minExpected := 5*time.Hour - 5*time.Second
	maxExpected := 5*time.Hour + 35*time.Second
	if *retryAfter < minExpected || *retryAfter > maxExpected {
		t.Fatalf("RetryAfter = %v, want between %v and %v (5h window)", *retryAfter, minExpected, maxExpected)
	}
}

func TestClaudeExecutor_RateLimit_BothRejectedUsesLongest(t *testing.T) {
	now := time.Now()
	fiveHourReset := now.Add(5 * time.Hour).Unix()
	sevenDayReset := now.Add(7 * 24 * time.Hour).Unix()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(fiveHourReset, 10))
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(sevenDayReset, 10))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Both limits exceeded."}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "claude-auth-1",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		},
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var rap retryAfterProvider
	if !errors.As(err, &rap) || rap == nil {
		t.Fatalf("expected error %T to implement RetryAfter() *time.Duration", err)
	}

	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		t.Fatalf("expected non-nil RetryAfter, got nil")
	}

	minExpected := 7*24*time.Hour - 5*time.Second
	maxExpected := 7*24*time.Hour + 35*time.Second
	if *retryAfter < minExpected || *retryAfter > maxExpected {
		t.Fatalf("RetryAfter = %v, want between %v and %v", *retryAfter, minExpected, maxExpected)
	}
}

func TestClaudeExecutor_RateLimit_CountTokensHonorsRateLimitReset(t *testing.T) {
	now := time.Now()
	sevenDayReset := now.Add(7 * 24 * time.Hour).Unix()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(sevenDayReset, 10))
		w.Header().Set("Retry-After", "604800")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"7-day rate limit exceeded."}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "claude-auth-1",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		},
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	ctx := cliproxyexecutor.WithUpstreamAttemptTracker(context.Background())
	_, err := executor.countTokensUpstream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err == nil {
		t.Fatal("expected error from CountTokens, got nil")
	}
	if !cliproxyexecutor.UpstreamAttempted(ctx) {
		t.Fatal("CountTokens() did not mark the HTTP request as an upstream attempt")
	}

	var rap retryAfterProvider
	if !errors.As(err, &rap) || rap == nil || rap.RetryAfter() == nil {
		t.Fatalf("expected CountTokens rate limit error to implement RetryAfter, got %v", err)
	}

	type credentialScopedProvider interface {
		IsCredentialScoped() bool
	}
	var csp credentialScopedProvider
	if !errors.As(err, &csp) || csp == nil || !csp.IsCredentialScoped() {
		t.Fatalf("expected CountTokens rate limit error to be credential-scoped, got %v", err)
	}

	minExpected := 7*24*time.Hour - 5*time.Second
	maxExpected := 7*24*time.Hour + 35*time.Second
	if *rap.RetryAfter() < minExpected || *rap.RetryAfter() > maxExpected {
		t.Fatalf("RetryAfter = %v, want between %v and %v", *rap.RetryAfter(), minExpected, maxExpected)
	}
}

func TestClaudeExecutor_RateLimit_CaseInsensitiveRawHeaderMap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["anthropic-ratelimit-unified-status"] = []string{"rejected"}
		w.Header()["anthropic-ratelimit-unified-7d-status"] = []string{"rejected"}
		w.Header()["anthropic-ratelimit-unified-7d-reset"] = []string{strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10)}
		w.Header()["retry-after"] = []string{"7200"}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Too many requests."}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "claude-auth-1",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		},
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var rap retryAfterProvider
	if !errors.As(err, &rap) || rap == nil || rap.RetryAfter() == nil {
		t.Fatalf("expected RetryAfter for non-canonical header map, got %v", err)
	}

	minExpected := 2*time.Hour - 5*time.Second
	maxExpected := 2*time.Hour + 35*time.Second
	if *rap.RetryAfter() < minExpected || *rap.RetryAfter() > maxExpected {
		t.Fatalf("RetryAfter = %v, want between %v and %v", *rap.RetryAfter(), minExpected, maxExpected)
	}
}

func TestClaudeExecutor_RateLimit_FastEntitlementWithRetryAfterRemainsRequestScoped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for fast mode."}}`))
	}))
	defer server.Close()

	cfg := &config.Config{DisableCooling: false}
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 2)

	executor := NewClaudeExecutor(cfg)
	manager.RegisterExecutor(executor)

	baseID := uuid.NewString()
	auth := &cliproxyauth.Auth{ID: baseID + "-fast-entitlement", Provider: "claude", Attributes: map[string]string{"api_key": "k1", "base_url": server.URL}}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-3-5-sonnet-20241022"}})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	payload := []byte(`{"speed":"fast","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	_, err := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	registeredAuth, ok := manager.GetByID(auth.ID)
	if !ok || registeredAuth == nil {
		t.Fatal("auth not found")
	}
	if registeredAuth.Unavailable || registeredAuth.Quota.Exceeded {
		t.Fatalf("fast entitlement refusal incorrectly cooled down the credential: unavailable=%v quota=%+v", registeredAuth.Unavailable, registeredAuth.Quota)
	}
}

func TestClaudeExecutor_AuthManager_CredentialScopeBlocksAllModelsAndAliases(t *testing.T) {
	var upstreamAttempts atomic.Int32
	now := time.Now()
	sevenDayReset := now.Add(7 * 24 * time.Hour).Unix()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAttempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(sevenDayReset, 10))
		w.Header().Set("Retry-After", strconv.FormatInt(7*24*3600, 10))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"7d limit rejected."}}`))
	}))
	defer server.Close()

	cfg := &config.Config{
		DisableCooling: false,
	}
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)

	executor := NewClaudeExecutor(cfg)
	manager.RegisterExecutor(executor)

	baseID := uuid.NewString()
	auth := &cliproxyauth.Auth{
		ID:       baseID + "-claude-cred",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		},
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{
		{ID: "claude-3-5-sonnet-20241022"},
		{ID: "claude-3-opus-20240229"},
		{ID: "claude-3-7-sonnet-20250219"},
	})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("failed to register auth: %v", errRegister)
	}

	// 1. Initial request on sonnet triggers 429 and records 7d cooldown
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	_, err := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err == nil {
		t.Fatal("expected error on first execute, got nil")
	}

	if attempts := upstreamAttempts.Load(); attempts != 1 {
		t.Fatalf("upstream attempts = %d, want 1", attempts)
	}

	// 2. Try requesting a completely different model (opus) on the same credential -> must be blocked locally
	_, errOpus := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   "claude-3-opus-20240229",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if errOpus == nil {
		t.Fatal("expected error for opus, got nil")
	}
	if attempts := upstreamAttempts.Load(); attempts != 1 {
		t.Fatalf("upstream attempts after opus = %d, want 1 (must be blocked locally)", attempts)
	}

	// 3. Try requesting a thinking suffix alias on the same credential -> must also be blocked locally
	_, errThinking := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   "claude-3-7-sonnet-20250219-thinking-16k",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if errThinking == nil {
		t.Fatal("expected error for thinking suffix, got nil")
	}
	if attempts := upstreamAttempts.Load(); attempts != 1 {
		t.Fatalf("upstream attempts after thinking suffix = %d, want 1 (must be blocked locally)", attempts)
	}

	// 4. Try streaming execution for opus on the same cooling credential -> must also be blocked locally
	_, errStream := manager.ExecuteStream(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   "claude-3-opus-20240229",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if errStream == nil {
		t.Fatal("expected error for streaming opus, got nil")
	}
	if attempts := upstreamAttempts.Load(); attempts != 1 {
		t.Fatalf("upstream attempts after streaming opus = %d, want 1 (must be blocked locally)", attempts)
	}
}

func TestClaudeExecutor_AuthManager_OrdinaryModel429DoesNotBlockSiblingModels(t *testing.T) {
	var attemptsSonnet atomic.Int32
	var attemptsOpus atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "claude-3-5-sonnet") {
			attemptsSonnet.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
			w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Model rate limit exceeded."}}`))
			return
		}
		attemptsOpus.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg-opus","type":"message","role":"assistant","content":[{"type":"text","text":"hello from opus"}]}`))
	}))
	defer server.Close()

	cfg := &config.Config{DisableCooling: false}
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)

	executor := NewClaudeExecutor(cfg)
	manager.RegisterExecutor(executor)

	baseID := uuid.NewString()
	auth := &cliproxyauth.Auth{
		ID:       baseID + "-ordinary-429",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		},
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{
		{ID: "claude-3-5-sonnet-20241022"},
		{ID: "claude-3-opus-20240229"},
	})
	t.Cleanup(func() {
		reg.UnregisterClient(auth.ID)
	})

	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	// 1. Initial request on sonnet triggers ordinary model 429
	payloadSonnet := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	_, errSonnet := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payloadSonnet,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if errSonnet == nil {
		t.Fatal("expected error on sonnet execute, got nil")
	}
	if attemptsSonnet.Load() != 1 {
		t.Fatalf("sonnet attempts = %d, want 1", attemptsSonnet.Load())
	}

	// 2. Request on opus MUST succeed on the same credential (not blocked by ordinary model-level 429)
	payloadOpus := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi opus"}]}]}`)
	respOpus, errOpus := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
		Model:   "claude-3-opus-20240229",
		Payload: payloadOpus,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if errOpus != nil {
		t.Fatalf("expected opus to succeed on same credential, got error: %v", errOpus)
	}
	if len(respOpus.Payload) == 0 {
		t.Fatal("expected non-empty response for opus")
	}
	if attemptsOpus.Load() != 1 {
		t.Fatalf("opus attempts = %d, want 1", attemptsOpus.Load())
	}
}

const ordinaryRefusalModel = "claude-3-5-sonnet-20241022"
const ordinaryRefusalDelay = 120 * time.Second

func ordinaryClaudeSingleCredentialRefusal(t *testing.T) (context.Context, *cliproxyauth.Auth, *int) {
	t.Helper()
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryClaudeTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Hostname() != "single-credential.fixture.invalid" || req.Method != http.MethodPost {
			return nil, errors.New("unexpected route for ordinary provider refusal")
		}
		headers := make(http.Header)
		headers.Set("Content-Type", "application/json")
		headers.Set("Retry-After", "120")
		headers.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     headers,
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"Request rate limit reached."}}`)),
			Request:    req,
		}, nil
	}))
	auth := &cliproxyauth.Auth{
		ID: uuid.NewString(), Provider: "claude",
		Attributes: map[string]string{"api_key": "ordinary-fixture-key", "base_url": "https://single-credential.fixture.invalid"},
	}
	return ctx, auth, &calls
}

func assertOrdinaryClaude429(t *testing.T, err error) {
	t.Helper()
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("expected HTTP 429, got %v", err)
	}
}

func assertOrdinaryClaudeProvider429(t *testing.T, err error) {
	t.Helper()
	assertOrdinaryClaude429(t, err)
	if !strings.Contains(err.Error(), "Request rate limit reached.") {
		t.Fatal("provider refusal body was lost")
	}
	var hint retryAfterProvider
	if !errors.As(err, &hint) || hint.RetryAfter() == nil || *hint.RetryAfter() != ordinaryRefusalDelay {
		t.Fatal("expected the provider's Retry-After unchanged")
	}
}

func TestOrdinaryClaudeProviderRateLimitKeepsWaitHint(t *testing.T) {
	ctx, auth, calls := ordinaryClaudeSingleCredentialRefusal(t)
	req := cliproxyexecutor.Request{
		Model:   ordinaryRefusalModel,
		Payload: []byte(`{"max_tokens":16,"messages":[{"role":"user","content":"ordinary request"}]}`),
	}
	response, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, req,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	assertOrdinaryClaudeProvider429(t, err)
	if len(response.Payload) != 0 || *calls != 1 {
		t.Fatal("provider refusal must end the request without a success body or retry")
	}
}

func TestOrdinaryClaudeIndependentRequestHonorsSameCredentialCooldown(t *testing.T) {
	ctx, auth, calls := ordinaryClaudeSingleCredentialRefusal(t)
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(NewClaudeExecutor(&config.Config{}))
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: ordinaryRefusalModel}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	if _, err := manager.Register(ctx, auth); err != nil {
		t.Fatal(err)
	}
	if members := manager.List(); len(members) != 1 || members[0].ID != auth.ID {
		t.Fatal("fixture must have exactly one eligible credential")
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
	providers := []string{"claude"}
	first := cliproxyexecutor.Request{Model: ordinaryRefusalModel,
		Payload: []byte(`{"max_tokens":16,"messages":[{"role":"user","content":"first independent input"}]}`)}
	started := time.Now()
	_, err := manager.Execute(ctx, providers, first, opts)
	assertOrdinaryClaude429(t, err)
	var hint retryAfterProvider
	if !errors.As(err, &hint) || hint.RetryAfter() == nil {
		t.Fatal("expected a retry deadline for the selected credential")
	}
	wait := *hint.RetryAfter()
	if wait <= 0 || wait > ordinaryRefusalDelay || wait < ordinaryRefusalDelay-time.Since(started) {
		t.Fatal("retry deadline did not reflect the provider's wait hint")
	}
	if *calls != 1 {
		t.Fatal("first refusal was retried")
	}
	second := cliproxyexecutor.Request{Model: ordinaryRefusalModel,
		Payload: []byte(`{"max_tokens":16,"messages":[{"role":"user","content":"new unrelated input"}]}`)}
	response, err := manager.Execute(ctx, providers, second, opts)
	assertOrdinaryClaude429(t, err)
	if len(response.Payload) != 0 || *calls != 1 {
		t.Fatal("independent request must respect the existing cooldown")
	}
}

func TestOrdinaryClaudeRateLimitStopsStreamBeforePayload(t *testing.T) {
	ctx, auth, calls := ordinaryClaudeSingleCredentialRefusal(t)
	req := cliproxyexecutor.Request{Model: ordinaryRefusalModel,
		Payload: []byte(`{"max_tokens":16,"messages":[{"role":"user","content":"ordinary streaming input"}]}`)}
	result, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(ctx, auth, req,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	assertOrdinaryClaudeProvider429(t, err)
	if result != nil || *calls != 1 {
		t.Fatal("pre-stream refusal produced stream output or retried")
	}
}
