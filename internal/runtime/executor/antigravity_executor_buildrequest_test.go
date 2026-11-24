package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	authsdk "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translation "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func ordinaryPortAuth() *authsdk.Auth {
	return &authsdk.Auth{
		ID: "ordinary-antigravity-port", Provider: "antigravity", Status: authsdk.StatusActive,
		Attributes: map[string]string{"base_url": "https://fixture.invalid"},
		Metadata:   map[string]any{"access_token": "fixture", "project_id": "fixture-project"},
	}
}

func captureOrdinaryNativeAntigravityRequest(t *testing.T, model, payload string) []byte {
	t.Helper()
	var body []byte
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(req *http.Request) (*http.Response, error) {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"response":{"candidates":[{"content":{"parts":[{"text":"fixture"}]},"finishReason":"STOP"}]}}`)), Request: req}, nil
	}))
	_, err := NewAntigravityExecutor(&config.Config{}).Execute(ctx, ordinaryPortAuth(),
		executorapi.Request{Model: model, Payload: []byte(payload)},
		executorapi.Options{SourceFormat: translation.FormatAntigravity, ResponseFormat: translation.FormatAntigravity})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestAntigravityBuildRequest_UsesRouteModelWhenPayloadContainsDifferentModel(t *testing.T) {
	body := captureOrdinaryNativeAntigravityRequest(t, "gemini-3-flash-agent", `{"model":"gemini-3.1-flash-lite","request":{"contents":[{"role":"user","parts":[{"text":"Perform a web search"}]}],"tools":[{"googleSearch":{}}]}}`)
	if got := ojson.GetBytesNoCopy(body, "model").String(); got != "gemini-3-flash-agent" {
		t.Fatalf("request model should stay on route model, got %q", got)
	}
	if ojson.GetBytesNoCopy(body, "project").String() != "fixture-project" ||
		!ojson.GetBytesNoCopy(body, "request.tools.0.googleSearch").IsObject() {
		t.Fatal("assigned project or ordinary declared tool was lost")
	}
}

func TestAntigravityBuildRequest_RejectsMissingProjectID(t *testing.T) {
	auth := ordinaryPortAuth()
	delete(auth.Metadata, "project_id")
	attempts := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(*http.Request) (*http.Response, error) {
		attempts++
		return nil, errors.New("unassigned project reached the transport")
	}))
	_, err := NewAntigravityExecutor(&config.Config{}).Execute(ctx, auth,
		executorapi.Request{Model: "gemini-3-flash-agent", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`)},
		executorapi.Options{SourceFormat: translation.FormatGemini})
	var status statusErr
	if attempts != 0 || !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest || !strings.Contains(status.Error(), "assigned project") {
		t.Fatalf("missing project did not fail locally: attempts=%d err=%v", attempts, err)
	}
}

func TestAntigravityBuildRequest_CaseInsensitiveClaudeCapsMaxOutputTokens(t *testing.T) {
	const payload = `{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":100000}}}`
	for _, model := range []string{"claude-sonnet-4-6", "Claude-Sonnet-4-6"} {
		t.Run(model, func(t *testing.T) {
			wire := captureOrdinaryNativeAntigravityRequest(t, model, payload)
			if n := ojson.GetBytesNoCopy(wire, "request.generationConfig.maxOutputTokens").Int(); n != 64000 {
				t.Fatalf("maxOutputTokens = %d, want 64000", n)
			}
		})
	}
}
