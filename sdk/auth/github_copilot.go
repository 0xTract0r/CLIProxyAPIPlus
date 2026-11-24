package auth

import (
	"context"
	"errors"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/copilot"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// GitHubCopilotAuthenticator accepts the historical login command name.
type GitHubCopilotAuthenticator struct{}

func NewGitHubCopilotAuthenticator() Authenticator { return &GitHubCopilotAuthenticator{} }

func (GitHubCopilotAuthenticator) Provider() string { return "github-copilot" }

// Legacy grants do not enter the shared refresh queue.
func (GitHubCopilotAuthenticator) RefreshLead() *time.Duration { return nil }

func (GitHubCopilotAuthenticator) Login(context.Context, *config.Config, *LoginOptions) (*coreauth.Auth, error) {
	return nil, errors.New("the legacy GitHub Copilot login is unavailable; use an explicitly configured supported Copilot integration")
}

// RefreshGitHubCopilotToken keeps the historical SDK signature and reports that
// an old grant must be replaced by the standard Copilot login.
func RefreshGitHubCopilotToken(_ context.Context, _ *config.Config, _ *copilot.CopilotTokenStorage) error {
	return errors.New("legacy GitHub Copilot token refresh is unavailable; use an explicitly configured supported Copilot integration")
}
