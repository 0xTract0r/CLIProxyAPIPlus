package cliproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryRetiredConnectorsRejectWithoutChangingStandardProviders(t *testing.T) {
	var _ interface{ GetWatcher() *WatcherWrapper } = (*Service)(nil)
	cfg := &config.Config{}
	var empty *Service
	if empty.GetWatcher() != nil {
		t.Fatal("nil SDK service unexpectedly has a watcher")
	}
	s := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
	for _, provider := range []string{"github-copilot", "gemini-cli"} {
		record := &coreauth.Auth{ID: "synthetic-" + provider, Provider: provider,
			Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "synthetic-old-grant"}}
		s.ensureExecutorsForAuth(record)
		got, found := s.coreManager.Executor(provider)
		if !found {
			t.Fatalf("missing explicit rejection for %s", provider)
		}
		if _, err := got.Execute(context.Background(), record, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}); !retiredConnectorStatus(err) {
			t.Fatalf("%s execute: %v", provider, err)
		}
		if _, err := got.ExecuteStream(context.Background(), record, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}); !retiredConnectorStatus(err) {
			t.Fatalf("%s stream: %v", provider, err)
		}
		if _, err := got.Refresh(context.Background(), record); !retiredConnectorStatus(err) {
			t.Fatalf("%s refresh: %v", provider, err)
		}
		if _, err := got.HttpRequest(context.Background(), record, nil); !retiredConnectorStatus(err) {
			t.Fatalf("%s request: %v", provider, err)
		}
	}
	for _, provider := range []string{"copilot", "gemini", "vertex"} {
		s.ensureExecutorsForAuth(&coreauth.Auth{ID: "synthetic-" + provider, Provider: provider})
		got, ok := s.coreManager.Executor(provider)
		if !ok {
			t.Fatalf("missing standard provider %s", provider)
		}
		switch provider {
		case "copilot":
			if _, isStandard := got.(*executor.CopilotExecutor); !isStandard {
				t.Fatalf("%s registered as %T", provider, got)
			}
		case "gemini":
			if _, isStandard := got.(*executor.GeminiExecutor); !isStandard {
				t.Fatalf("%s registered as %T", provider, got)
			}
		case "vertex":
			if _, isStandard := got.(*executor.GeminiVertexExecutor); !isStandard {
				t.Fatalf("%s registered as %T", provider, got)
			}
		}
	}

	endpoint := "https://explicit-compat.example.invalid/v1"
	cfg.OpenAICompatibility = []config.OpenAICompatibility{{
		Name:          "explicit-fixture",
		BaseURL:       endpoint,
		Models:        []config.OpenAICompatibilityModel{{Name: "fixture-model", Alias: "fixture-alias"}},
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "fixture-api-key"}},
	}}
	configured, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config: cfg, IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil || len(configured) != 1 {
		t.Fatalf("explicit configuration produced %d credentials: %v", len(configured), err)
	}
	s.ensureExecutorsForAuth(configured[0])
	got, ok := s.coreManager.Executor(configured[0].Provider)
	if !ok {
		t.Fatalf("explicit compatibility provider %q was not registered", configured[0].Provider)
	}
	compat, compatible := got.(*executor.OpenAICompatExecutor)
	if !compatible {
		t.Fatalf("explicit compatibility provider became %T", got)
	}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", explicitConnectorFixtureTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != endpoint+"/chat/completions" || req.Header.Get("Authorization") != "Bearer fixture-api-key" {
			t.Fatal("explicit config provenance was lost")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"object":"chat.completion","choices":[{"message":{"role":"assistant","content":"explicit result"}}]}`))}, nil
	}))
	response, err := compat.Execute(ctx, configured[0], cliproxyexecutor.Request{
		Model: "fixture-model", Payload: []byte(`{"messages":[{"role":"user","content":"synthetic request"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil || !strings.Contains(string(response.Payload), "explicit result") {
		t.Fatalf("explicit compatibility execution: %v, %s", err, response.Payload)
	}

	authenticator := sdkauth.NewGitHubCopilotAuthenticator()
	if authenticator.Provider() != "github-copilot" || authenticator.RefreshLead() != nil {
		t.Fatal("historical login ABI or refresh policy changed")
	}
	if err := sdkauth.RefreshGitHubCopilotToken(context.Background(), cfg, nil); err == nil {
		t.Fatal("legacy refresh reported success")
	}
	if granted, err := authenticator.Login(context.Background(), cfg, nil); granted != nil || err == nil ||
		!strings.Contains(err.Error(), "explicitly configured") {
		t.Fatalf("retired CLI grant: auth=%v err=%v", granted, err)
	}
}

func retiredConnectorStatus(err error) bool {
	var status cliproxyexecutor.StatusError
	return err != nil && errors.As(err, &status) && status.StatusCode() == http.StatusNotImplemented && coreauth.IsRequestStopError(err)
}

type explicitConnectorFixtureTransport func(*http.Request) (*http.Response, error)

func (f explicitConnectorFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
