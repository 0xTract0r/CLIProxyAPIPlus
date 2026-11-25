package executor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCodexCacheOrdinaryScopesAndDerivedSession(t *testing.T) {
	cache := func(tenant, token, origin, model, session, raw string) string {
		ctx := t.Context()
		if tenant != "" {
			g, _ := gin.CreateTestContext(nil)
			g.Set("userApiKey", tenant)
			ctx = context.WithValue(ctx, "gin", g)
		}
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": token, "base_url": origin}}
		req := cliproxyexecutor.Request{Payload: []byte(raw)}
		opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: session}}
		doc := ojson.NewDoc(req.Payload)
		ordinaryCodexPromptCache(ctx, doc, auth, model, req, opts)
		got := doc.Get("prompt_cache_key").String()
		if got != "" && (len(got) != 64 || strings.Contains(got, "literal-cache") || got == helps.ProviderSessionUUID("codex", opts.Metadata)) {
			t.Fatal("raw cache identity escaped the scope")
		}
		return got
	}
	const base = "https://fixture.invalid"
	const payload = `{"prompt_cache_key":"literal-cache"}`
	stable := cache("tenant", "credential", base, "model", "first", payload)
	if stable == "" || stable != cache("tenant", "credential", base, "model", "second", payload) {
		t.Fatal("explicit cache key lost stability")
	}
	for _, other := range []string{
		cache("other", "credential", base, "model", "first", payload),
		cache("tenant", "new-credential", base, "model", "first", payload),
		cache("tenant", "credential", "https://other.invalid", "model", "first", payload),
		cache("tenant", "credential", base, "alternate-model", "first", payload),
	} {
		if other == "" || other == stable {
			t.Fatal("tenant, credential, endpoint, or model failed to partition cache")
		}
	}
	first := cache("tenant", "credential", base, "model", "first", `{}`)
	if first == "" || first == cache("tenant", "credential", base, "model", "second", `{}`) {
		t.Fatal("execution session scope failed")
	}
	if cache("", "credential", base, "model", "first", payload) != "" ||
		cache("tenant", "", base, "model", "first", payload) != "" ||
		cache("tenant", "credential", base, "model", "", `{"metadata":{"user_id":"bare-user"}}`) != "" {
		t.Fatal("unscoped cache was forwarded")
	}
}

func TestCodexOrdinaryOAuthHeaderIsBoundToSelectedOrigin(t *testing.T) {
	e := NewCodexExecutor(&config.Config{})
	for _, test := range []struct {
		name, base, target string
		token, account     string
		status             int
	}{
		{"ordinary upstream", "", "https://chatgpt.com/backend-api/codex/responses", "fixture", "account", 0},
		{"custom same origin", "https://fixture.invalid", "https://fixture.invalid/responses", "fixture", "", 0},
		{"foreign origin", "https://fixture.invalid", "https://other.invalid/responses", "fixture", "", 403},
		{"missing bearer", "https://fixture.invalid", "https://fixture.invalid/responses", "", "", 401},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := &cliproxyauth.Auth{
				Attributes: map[string]string{"base_url": test.base},
				Metadata:   map[string]any{"access_token": test.token, "account_id": "account"},
			}
			r, err := http.NewRequest(http.MethodPost, test.target, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = e.PrepareRequest(r, a)
			if test.status != 0 {
				var status statusErr
				if !errors.As(err, &status) || status.StatusCode() != test.status || r.Header.Get("Authorization") != "" {
					t.Fatalf("credential escaped preflight: %v", err)
				}
				return
			}
			if err != nil || r.Header.Get("Authorization") != "Bearer "+test.token || r.Header.Get("ChatGPT-Account-ID") != test.account {
				t.Fatalf("incorrect same-origin headers: %v", err)
			}
		})
	}
}

func TestCodexCacheHTTPWSShareTrustedExecutionScope(t *testing.T) {
	g, _ := gin.CreateTestContext(nil)
	g.Set("userApiKey", "fixture-tenant")
	ctx := context.WithValue(t.Context(), "gin", g)
	cfg := &config.Config{}
	e := NewCodexExecutor(cfg)
	ws := NewCodexWebsocketsExecutor(cfg)
	a := &cliproxyauth.Auth{ID: "fixture-credential", Attributes: map[string]string{"api_key": "opaque-fixture", "base_url": "https://fixture.invalid"}}
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(`{"model":"gpt-5","system":"system","messages":[{"role":"user","content":"fixture"}],"metadata":{"user_id":"bare-user"}}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "trusted-execution"}}
	httpWire, err := e.prepareOrdinaryCodexBody(ctx, a, req, opts, true, false)
	if err != nil {
		t.Fatal(err)
	}
	wsWire, err := ws.prepareCodexWebsocketStream(ctx, a, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	cache := ojson.GetBytesNoCopy(httpWire, "prompt_cache_key").String()
	if !ojson.ValidBytes(wsWire.clientBody) || len(cache) != 64 || ojson.GetBytesNoCopy(wsWire.clientBody, "prompt_cache_key").String() != cache {
		t.Fatal("HTTP and WebSocket did not share the ordinary trusted cache scope")
	}
	for _, name := range []string{"Session-Id", "Session_id", "Conversation-Id"} {
		if wsWire.wsHeaders.Get(name) != "" {
			t.Fatalf("cache was promoted to %s header", name)
		}
	}
}
