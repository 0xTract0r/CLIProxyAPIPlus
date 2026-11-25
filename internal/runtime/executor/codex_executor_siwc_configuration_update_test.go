package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MoeForge/moego/ojson"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// SIWC (Sign in with ChatGPT) subscriptions reject configuration_update input
// items with subscription_sharing_unsupported_capability, so the SIWC body
// rules must drop every such item and promote the last nonempty effort into
// the top-level reasoning.effort even though that invalidates the prompt cache.
const ordinaryCodexUpdatePromptCacheKey = "siwc-update-session"
const ordinaryCodexUpdatePayload = `{"model":"gpt-6-astra","prompt_cache_key":"` + ordinaryCodexUpdatePromptCacheKey + `","reasoning":{"effort":"medium","summary":"auto"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"configuration_update","reasoning":{"effort":"high"}}]}`

func TestCodexExecutorKeepsConfigurationUpdates(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_oauth_update","object":"response","status":"completed","model":"gpt-6-astra","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	_, err := executor.Execute(context.Background(), codexOAuthTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gpt-6-astra",
		Payload: []byte(ordinaryCodexUpdatePayload),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	updates := 0
	for _, item := range ojson.GetBytes(gotBody, "input").Array() {
		if item.Get("type").String() == "configuration_update" {
			updates++
		}
	}
	if updates != 2 {
		t.Fatalf("non-SIWC upstream configuration_update count = %d, want 2; body=%s", updates, gotBody)
	}
	if effort := ojson.GetBytes(gotBody, "reasoning.effort").String(); effort != "medium" {
		t.Fatalf("non-SIWC upstream reasoning.effort = %q, want pinned %q; body=%s", effort, "medium", gotBody)
	}
}
