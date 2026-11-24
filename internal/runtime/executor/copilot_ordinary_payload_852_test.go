package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/copilot"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestCopilotOrdinaryCanonicalPayloadSelector(t *testing.T) {
	const id = "claude-opus-5"
	auth := copilotOrdinaryFixtureAuth(t, "selected", id, copilot.Chat)
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{
		{Models: []config.PayloadModelRule{{Name: "copilot/" + id}}, Params: map[string]any{"temperature": 0.25}},
		{Models: []config.PayloadModelRule{{Name: "copilot/other"}}, Params: map[string]any{"max_tokens": 999}},
	}}}
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", copilotOrdinaryFixtureTransport(func(r *http.Request) (*http.Response, error) {
		wire, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if q := ojson.GetBytesNoCopy(wire, "temperature"); !q.Exists() || q.Float() != 0.25 {
			t.Error("registered canonical selector failed to update temperature")
		}
		if ojson.GetBytesNoCopy(wire, "max_tokens").Exists() {
			t.Error("unrelated model selector matched")
		}
		if ojson.GetBytesNoCopy(wire, "model").String() != id {
			t.Error("canonical selector changed wire model")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)), Request: r}, nil
	}))
	req := executorapi.Request{Model: "copilot/" + id, Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}
	opts := executorapi.Options{SourceFormat: sdktranslator.FormatOpenAI, Metadata: map[string]any{executorapi.RequestedModelMetadataKey: "configured-alias"}}
	if _, err := NewCopilotExecutor(cfg).Execute(ctx, auth, req, opts); err != nil {
		t.Fatal(err)
	}
}
