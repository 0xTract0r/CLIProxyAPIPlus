package executor

import (
	"context"
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

func TestOrdinaryAntigravitySendsLegalToolSchemaWithSelectedCredential(t *testing.T) {
	const request = `{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"OBJECT","properties":{"id":{"type":"STRING","description":"A public record id"},"region":{"type":"STRING","enum":["us","eu"]}},"required":["id"]}}]}]}}`
	for _, count := range []bool{false, true} {
		name := "generate"
		if count {
			name = "count"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(r *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if r.URL.Host != "fixture.invalid" || r.Header.Get("Authorization") != "Bearer explicit-fixture-token" {
					t.Fatal("unexpected upstream or credential")
				}
				schema := ojson.GetBytesNoCopy(body, "request.tools.0.functionDeclarations.0.parameters")
				if schema.Get("properties.id.description").String() != "A public record id" ||
					schema.Get("required.0").String() != "id" || schema.Get("properties.region.enum.1").String() != "eu" {
					t.Fatal("ordinary tool schema changed")
				}
				result := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}]}}`
				if count {
					if r.URL.Path != antigravityCountTokensPath || ojson.GetBytesNoCopy(body, "request.model").String() != "models/fixture-model" {
						t.Fatal("count request did not use the selected model")
					}
					result = `{"totalTokens":11}`
				} else if r.URL.Path != antigravityGeneratePath || ojson.GetBytesNoCopy(body, "project").String() != "explicit-fixture-project" {
					t.Fatal("generation did not use the explicit assigned project")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(result)), Request: r}, nil
			}))
			auth := &authsdk.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"},
				Metadata: map[string]any{"access_token": "explicit-fixture-token", "project_id": "explicit-fixture-project"}}
			exec := NewAntigravityExecutor(&config.Config{})
			run := exec.Execute
			if count {
				run = exec.CountTokens
			}
			_, err := run(ctx, auth, executorapi.Request{Model: "fixture-model", Payload: []byte(request)},
				executorapi.Options{SourceFormat: translation.FormatAntigravity, ResponseFormat: translation.FormatAntigravity})
			if err != nil || calls != 1 {
				t.Fatalf("normal request failed: calls=%d, err=%v", calls, err)
			}
		})
	}
}
