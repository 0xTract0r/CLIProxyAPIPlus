package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// TestNewCodexStatusErr_RateLimitHeaderFallback covers the unit-level contract of
// the 429 classification fix: when the body carries no usage-limit signal but the
// Codex rate-limit headers report an exhausted window, the synthesized statusErr
// must surface the header-derived retry-after so MarkResult applies a real
// (plan-quota) cooldown instead of the 1-minute transient window. Transient 429s
// (no exhausted header) and non-429 statuses must keep returning nil.
func TestNewCodexStatusErr_RateLimitHeaderFallback(t *testing.T) {
	transientBody := []byte(`{"error":{"type":"rate_limit","message":"TPM burst, retry shortly"}}`)
	rateLimitBody := []byte(`{"error":{"type":"rate_limit_reached","message":"You've hit your usage limit."}}`)

	exhausted := http.Header{}
	exhausted.Set("X-Codex-Secondary-Used-Percent", "100")
	exhausted.Set("X-Codex-Secondary-Reset-After-Seconds", "8000")

	notExhausted := http.Header{}
	notExhausted.Set("X-Codex-Secondary-Used-Percent", "77")
	notExhausted.Set("X-Codex-Secondary-Reset-After-Seconds", "8000")

	tests := []struct {
		name       string
		status     int
		body       []byte
		headers    http.Header
		wantSet    bool
		wantApprox time.Duration
	}{
		{name: "exhausted header drives long cooldown", status: http.StatusTooManyRequests, body: rateLimitBody, headers: exhausted, wantSet: true, wantApprox: 8000 * time.Second},
		{name: "transient 429 without exhausted header stays nil", status: http.StatusTooManyRequests, body: transientBody, headers: notExhausted, wantSet: false},
		{name: "transient 429 without headers stays nil", status: http.StatusTooManyRequests, body: transientBody, headers: nil, wantSet: false},
		{name: "non-429 never uses header fallback", status: http.StatusInternalServerError, body: rateLimitBody, headers: exhausted, wantSet: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := newCodexStatusErr(tc.status, tc.body, tc.headers)
			got := err.RetryAfter()
			if tc.wantSet {
				if got == nil {
					t.Fatalf("RetryAfter() = nil, want ~%s", tc.wantApprox)
				}
				if *got < tc.wantApprox-time.Minute || *got > tc.wantApprox+time.Minute {
					t.Fatalf("RetryAfter() = %s, want ~%s", *got, tc.wantApprox)
				}
			} else if got != nil {
				t.Fatalf("RetryAfter() = %s, want nil", *got)
			}
		})
	}
}

// TestCodexRateLimitRetryAfterFromHeaders covers the header parser directly,
// including the unix Reset-At form and the latest-window-wins behavior.
func TestCodexRateLimitRetryAfterFromHeaders(t *testing.T) {
	now := time.Unix(1_780_000_000, 0)

	t.Run("reset-after-seconds", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Codex-Primary-Used-Percent", "100")
		h.Set("X-Codex-Primary-Reset-After-Seconds", "3600")
		got := codexRateLimitRetryAfterFromHeaders(h, now)
		if got == nil || *got != time.Hour {
			t.Fatalf("got %v, want 1h", got)
		}
	})

	t.Run("reset-at unix and latest window wins", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Codex-Primary-Used-Percent", "100")
		h.Set("X-Codex-Primary-Reset-After-Seconds", "3600")
		h.Set("X-Codex-Secondary-Used-Percent", "100")
		h.Set("X-Codex-Secondary-Reset-At", strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10))
		got := codexRateLimitRetryAfterFromHeaders(h, now)
		if got == nil || *got < 47*time.Hour || *got > 49*time.Hour {
			t.Fatalf("got %v, want ~48h (latest window)", got)
		}
	})

	t.Run("not exhausted returns nil", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Codex-Primary-Used-Percent", "99")
		h.Set("X-Codex-Primary-Reset-After-Seconds", "3600")
		if got := codexRateLimitRetryAfterFromHeaders(h, now); got != nil {
			t.Fatalf("got %v, want nil (window not exhausted)", got)
		}
	})

	t.Run("empty headers return nil", func(t *testing.T) {
		if got := codexRateLimitRetryAfterFromHeaders(nil, now); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})
}

// TestCodexExecutor_RateLimitHeader429_ParksExhaustedAuth_RotatesToHealthy is the
// end-to-end coverage for the production incident: a Plus account whose weekly
// window is fully consumed returns a `rate_limit_reached` 429 carrying the reset
// window in headers. The fix must cool that credential down for its real reset
// window (plan-quota, not the 1-minute transient) so round-robin parks it and
// concentrates traffic on the healthy Pro account — the client must keep getting
// 200s instead of consecutive 429s.
func TestCodexExecutor_RateLimitHeader429_ParksExhaustedAuth_RotatesToHealthy(t *testing.T) {
	provider := "codex"
	model := "gpt-5.4-mini"
	exhaustedID := "codex-plus-exhausted"
	healthyID := "codex-pro-healthy"
	exhaustedKey := "key-" + exhaustedID
	healthyKey := "key-" + healthyID

	var hitsExhausted, hitsHealthy atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer " + exhaustedKey:
			hitsExhausted.Add(1)
			// Weekly window fully consumed; body uses rate_limit_reached (not
			// usage_limit_reached) and the reset window lives in the headers.
			w.Header().Set("X-Codex-Secondary-Used-Percent", "100")
			w.Header().Set("X-Codex-Secondary-Reset-After-Seconds", "8000")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_reached","message":"You've hit your usage limit."}}`))
		case "Bearer " + healthyKey:
			hitsHealthy.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1775555723,\"status\":\"completed\",\"model\":\"" + model + "\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":28,\"total_tokens\":36}}}\n\n"))
		default:
			http.Error(w, "unexpected auth", http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(NewCodexExecutor(&config.Config{}))

	reg := registry.GetGlobalRegistry()
	for _, id := range []string{exhaustedID, healthyID} {
		reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
	}
	t.Cleanup(func() {
		for _, id := range []string{exhaustedID, healthyID} {
			reg.UnregisterClient(id)
		}
	})

	for id, key := range map[string]string{exhaustedID: exhaustedKey, healthyID: healthyKey} {
		auth := &cliproxyauth.Auth{
			ID:       id,
			Provider: provider,
			Attributes: map[string]string{
				"base_url": server.URL,
				"api_key":  key,
			},
		}
		if _, errReg := manager.Register(context.Background(), auth); errReg != nil {
			t.Fatalf("Register(%s) error = %v", id, errReg)
		}
		manager.RefreshSchedulerEntry(id)
	}

	doRequest := func() (cliproxyexecutor.Response, error) {
		return manager.Execute(
			context.Background(),
			[]string{provider},
			cliproxyexecutor.Request{
				Model:   model,
				Payload: []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"Say ok"}]}`),
			},
			cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("openai"),
				Stream:       false,
			},
		)
	}

	// Round-robin will reach the exhausted credential within the first couple of
	// requests; on that request the conductor must rotate to the healthy account
	// and still return 200. Every request must succeed — the client never sees 429.
	for i := 0; i < 4; i++ {
		resp, err := doRequest()
		if err != nil {
			t.Fatalf("request %d error = %v, want nil (rotation must reach healthy account)", i, err)
		}
		if gotContent := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); gotContent != "ok" {
			t.Fatalf("request %d content = %q, want %q; payload=%s", i, gotContent, "ok", string(resp.Payload))
		}
	}

	// The exhausted credential must have been parked under a real plan-quota
	// cooldown (~8000s), not the 1-minute transient window.
	got, ok := manager.GetByID(exhaustedID)
	if !ok || got == nil {
		t.Fatalf("GetByID(%s) = (%v, %v)", exhaustedID, got, ok)
	}
	state, exists := got.ModelStates[model]
	if !exists || state == nil {
		t.Fatalf("%s: ModelStates[%s] missing, want state set by header-driven 429", exhaustedID, model)
	}
	if !state.Quota.Exceeded {
		t.Fatalf("%s: ModelStates[%s].Quota.Exceeded = false, want true (header window exhausted is plan-quota, not transient)", exhaustedID, model)
	}
	if state.Quota.Reason != "quota" {
		t.Fatalf("%s: ModelStates[%s].Quota.Reason = %q, want %q", exhaustedID, model, state.Quota.Reason, "quota")
	}
	// Cooldown must be far longer than the 1-minute transient window.
	if remaining := time.Until(state.NextRetryAfter); remaining < time.Hour {
		t.Fatalf("%s: NextRetryAfter remaining = %s, want >= 1h (≈8000s reset window)", exhaustedID, remaining)
	}

	// Exhausted credential is hit at most once, then parked; healthy account
	// carries the rest of the traffic.
	if got := hitsExhausted.Load(); got > 1 {
		t.Fatalf("exhausted stub hits = %d, want <= 1 (must be parked after first 429)", got)
	}
	if got := hitsHealthy.Load(); got < 4 {
		t.Fatalf("healthy stub hits = %d, want >= 4 (all client requests served)", got)
	}
}
