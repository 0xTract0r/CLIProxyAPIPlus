package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// These tests guard the credits-aware 429 classification: a credit-backed Codex
// account whose main plan window reports 100% must not be parked until the
// window reset by a transient (capacity / TPM) 429, while an explicit usage-limit
// 429 still cools it to the reset (the backstop for optimistic credit headers).

const (
	codexCapacity429Body   = `{"error":{"message":"Selected model is at capacity. Please try a different model."}}`
	codexRateLimit429Body  = `{"error":{"type":"rate_limit","message":"TPM burst, retry shortly"}}`
	codexUsageLimit429Body = `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`
)

func codexExhaustedWindowHeaders(prefix string, resetAt time.Time, withCredits bool) http.Header {
	h := http.Header{}
	h.Set(prefix+"-Primary-Used-Percent", "100")
	h.Set(prefix+"-Primary-Reset-At", strconv.FormatInt(resetAt.Unix(), 10))
	h.Set(prefix+"-Secondary-Used-Percent", "0")
	if withCredits {
		h.Set("X-Codex-Credits-Has-Credits", "True")
		h.Set("X-Codex-Credits-Balance", "59897.4803850000")
		h.Set("X-Codex-Credits-Unlimited", "False")
	}
	return h
}

func withReachedType(h http.Header) http.Header {
	h.Set("X-Codex-Rate-Limit-Reached-Type", "primary")
	return h
}

// TestNewCodexStatusErr_CreditsHeadersIgnoredOutside429 pins that the credits /
// header-window logic only runs for 429s: a 5xx carrying exhausted + credit
// headers gets no header-derived retry-after.
func TestNewCodexStatusErr_CreditsHeadersIgnoredOutside429(t *testing.T) {
	headers := codexExhaustedWindowHeaders("X-Codex", time.Now().Add(5*24*time.Hour), true)
	err := newCodexStatusErr(http.StatusInternalServerError, []byte(`{"error":{"message":"boom"}}`), headers)
	if got := err.RetryAfter(); got != nil {
		t.Fatalf("RetryAfter() = %s, want nil for a non-429 status", *got)
	}
}

func TestNewCodexStatusErr_CreditsAware429(t *testing.T) {
	window := 5 * 24 * time.Hour
	resetAt := time.Now().Add(window)

	tests := []struct {
		name    string
		body    string
		headers http.Header
		// wantWindow: retry-after must be ~the window reset; otherwise it must be
		// nil (transient 429 handling).
		wantWindow bool
	}{
		{name: "capacity 429 with credits is transient", body: codexCapacity429Body, headers: codexExhaustedWindowHeaders("X-Codex", resetAt, true), wantWindow: false},
		{name: "rate_limit 429 with credits is transient", body: codexRateLimit429Body, headers: codexExhaustedWindowHeaders("X-Codex", resetAt, true), wantWindow: false},
		{name: "usage_limit_reached 429 with credits still cools to reset", body: codexUsageLimit429Body, headers: codexExhaustedWindowHeaders("X-Codex", resetAt, true), wantWindow: true},
		{name: "capacity 429 without credits keeps old window cooldown", body: codexCapacity429Body, headers: codexExhaustedWindowHeaders("X-Codex", resetAt, false), wantWindow: true},
		{name: "rate_limit 429 without credits keeps old window cooldown", body: codexRateLimit429Body, headers: codexExhaustedWindowHeaders("X-Codex", resetAt, false), wantWindow: true},
		{name: "bengalfox exhausted with credits still cools", body: codexCapacity429Body, headers: codexExhaustedWindowHeaders("X-Codex-Bengalfox", resetAt, true), wantWindow: true},
		{name: "real usage_limit_reached sample with credits still cools", body: `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"pro","resets_at":` + strconv.FormatInt(resetAt.Unix(), 10) + `}}`, headers: codexExhaustedWindowHeaders("X-Codex", resetAt, true), wantWindow: true},
		{name: "rate_limit_reached body with credits still cools", body: `{"error":{"type":"rate_limit_reached"}}`, headers: codexExhaustedWindowHeaders("X-Codex", resetAt, true), wantWindow: true},
		{name: "reached-type header with credits still cools", body: codexCapacity429Body, headers: withReachedType(codexExhaustedWindowHeaders("X-Codex", resetAt, true)), wantWindow: true},
		{name: "capacity body with credits and no reached-type header is transient", body: codexCapacity429Body, headers: codexExhaustedWindowHeaders("X-Codex", resetAt, true), wantWindow: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := newCodexStatusErr(http.StatusTooManyRequests, []byte(tc.body), tc.headers)
			if err.StatusCode() != http.StatusTooManyRequests {
				t.Fatalf("StatusCode() = %d, want 429", err.StatusCode())
			}
			got := err.RetryAfter()
			if tc.wantWindow {
				if got == nil || *got < window-time.Hour || *got > window+time.Hour {
					t.Fatalf("RetryAfter() = %v, want ~%s (window reset)", got, window)
				}
				return
			}
			if got != nil {
				t.Fatalf("RetryAfter() = %s, want nil (transient 429, not the window reset)", *got)
			}
		})
	}
}

// TestCodexExecutor_Capacity429WithCredits_NoMultiDayCooldown is the end-to-end
// regression for the production lock: a credit-backed account at 100% weekly usage
// hitting one capacity 429 must get the short transient cooldown, not a
// multi-day plan-quota cooldown.
func TestCodexExecutor_Capacity429WithCredits_NoMultiDayCooldown(t *testing.T) {
	provider := "codex"
	model := "gpt-5.4-mini"
	authID := "codex-pro-credits-capacity-429"
	apiKey := "key-" + authID

	resetAt := time.Now().Add(5 * 24 * time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+apiKey {
			http.Error(w, "unexpected auth", http.StatusUnauthorized)
			return
		}
		for key, values := range codexExhaustedWindowHeaders("X-Codex", resetAt, true) {
			for _, v := range values {
				w.Header().Add(key, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(codexCapacity429Body))
	}))
	defer server.Close()

	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(NewCodexExecutor(&config.Config{}))
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       authID,
		Provider: provider,
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  apiKey,
		},
	}
	if _, errReg := manager.Register(context.Background(), auth); errReg != nil {
		t.Fatalf("Register(%s) error = %v", authID, errReg)
	}
	manager.RefreshSchedulerEntry(authID)

	_, errExec := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{
			Model:   model,
			Payload: []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"Say ok"}]}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")},
	)
	if errExec == nil {
		t.Fatal("Execute() error = nil, want the upstream 429")
	}

	got, ok := manager.GetByID(authID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) = (%v, %v)", authID, got, ok)
	}
	state := got.ModelStates[model]
	if state == nil {
		t.Fatalf("ModelStates[%s] missing, want state set by the 429", model)
	}
	if state.Quota.Exceeded {
		t.Fatalf("ModelStates[%s].Quota.Exceeded = true, want false (credit-backed capacity 429 is transient)", model)
	}
	if remaining := time.Until(state.NextRetryAfter); remaining > 10*time.Minute {
		t.Fatalf("NextRetryAfter remaining = %s, want a short transient cooldown, not the ~5d window reset", remaining)
	}
}
