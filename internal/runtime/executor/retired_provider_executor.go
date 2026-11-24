package executor

import (
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Retired provider executors preserve actionable errors for existing records.
type retiredProviderExecutor struct {
	provider string
	message  string
}

type GitHubCopilotExecutor struct{ retiredProviderExecutor }

func NewGitHubCopilotExecutor(_ *config.Config) *GitHubCopilotExecutor {
	return &GitHubCopilotExecutor{retiredProviderExecutor{
		provider: "github-copilot",
		message:  "this GitHub Copilot credential uses the retired integration; configure a supported Copilot integration or a permitted OpenAI-compatible endpoint",
	}}
}

type GeminiCLIExecutor struct{ retiredProviderExecutor }

func NewGeminiCLIExecutor(_ *config.Config) *GeminiCLIExecutor {
	return &GeminiCLIExecutor{retiredProviderExecutor{
		provider: "gemini-cli",
		message:  "Gemini CLI sign-in credentials are unavailable for inference in this gateway; configure Gemini API or Vertex AI",
	}}
}

func (e retiredProviderExecutor) Identifier() string { return e.provider }

func (e retiredProviderExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.unavailable()
}

func (e retiredProviderExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, e.unavailable()
}

func (e retiredProviderExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, e.unavailable()
}

func (e retiredProviderExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, e.unavailable()
}

func (e retiredProviderExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, e.unavailable()
}

func (e retiredProviderExecutor) PrepareRequest(*http.Request, *coreauth.Auth) error {
	return e.unavailable()
}

func (e retiredProviderExecutor) unavailable() error {
	return retiredProviderError{statusErr{code: http.StatusNotImplemented, msg: e.message}}
}

type retiredProviderError struct{ statusErr }

func (retiredProviderError) IsRequestStop() bool { return true }
