package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	authsdk "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translation "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryAntigravityNonStreamResponseSSEOnSameRequest(t *testing.T) {
	const first = "data: {\"response\":{\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"first\"}]}}]}}\n\n"
	const last = "data: {\"response\":{\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"last\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"totalTokenCount\":5}}}\n\n"
	for _, tc := range []struct {
		name, wire, contentType string
		wantStatus              int
	}{
		{"accumulate content and usage", first + last, "text/event-stream; charset=utf-8", http.StatusOK},
		{"recover a malformed frame", "data: {\"incomplete\"\n\n" + first + last, "text/event-stream", http.StatusOK},
		{"in-band HTTP refusal", first + "data: {\"error\":{\"code\":429,\"message\":\"fixture wait\"}}\n\n", "text/event-stream", http.StatusTooManyRequests},
		{"truncated SSE", first, "text/event-stream", http.StatusBadGateway},
		{"SSE-looking non-SSE", first + last, "application/json", http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sends := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(req *http.Request) (*http.Response, error) {
				sends++
				if req.URL.Host != "fixture.invalid" || req.URL.Path != antigravityGeneratePath ||
					req.Header.Get("Authorization") != "Bearer fixture" {
					t.Fatal("non-stream request moved to a different endpoint or credential")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {tc.contentType}, "Retry-After": {"9"}},
					Body: io.NopCloser(strings.NewReader(tc.wire)), Request: req}, nil
			}))
			exec := NewAntigravityExecutor(&config.Config{})
			auth := &authsdk.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"}, Metadata: map[string]any{"project_id": "explicit-project", "access_token": "fixture"}}
			req := executorapi.Request{Model: "fixture-model", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`)}
			response, err := exec.Execute(ctx, auth, req, executorapi.Options{SourceFormat: translation.FormatGemini, ResponseFormat: translation.FormatAntigravity})
			if sends != 1 {
				t.Fatalf("sent %d requests", sends)
			}
			if tc.wantStatus == http.StatusOK {
				if err != nil ||
					ojson.GetBytesNoCopy(response.Payload, "response.candidates.0.content.parts.0.text").String() != "first" ||
					ojson.GetBytesNoCopy(response.Payload, "response.candidates.0.content.parts.1.text").String() != "last" ||
					ojson.GetBytesNoCopy(response.Payload, "response.usageMetadata.totalTokenCount").Int() != 5 {
					t.Fatalf("ordinary assembled response = %s, err = %v", response.Payload, err)
				}
				return
			}
			var status statusErr
			if !errors.As(err, &status) || status.StatusCode() != tc.wantStatus {
				t.Fatalf("expected status %d, got %v", tc.wantStatus, err)
			}
			if tc.wantStatus == http.StatusTooManyRequests && (status.RetryAfter() == nil || *status.RetryAfter() != 9*time.Second) {
				t.Fatalf("upstream retry metadata was lost: %v", status.RetryAfter())
			}
		})
	}
}
