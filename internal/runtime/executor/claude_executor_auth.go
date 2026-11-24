package executor

import (
	"context"
	"net/http"
	"strings"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func ordinaryClaudeRefreshToken(auth *cliproxyauth.Auth) string {
	if auth == nil || strings.TrimSpace(auth.Attributes["api_key"]) != "" {
		return ""
	}
	if token := claudeauth.ReadMetadataString(&auth.Metadata, "refresh_token"); strings.TrimSpace(token) != "" {
		return strings.TrimSpace(token)
	}
	return strings.TrimSpace(claudeauth.ReadMetadataString(&auth.Metadata, "refreshToken"))
}

// ShouldPrepareRequestAuth only selects an expired ordinary OAuth credential
// that actually has a rotation token. API keys and setup tokens are left as-is.
func (e *ClaudeExecutor) ShouldPrepareRequestAuth(auth *cliproxyauth.Auth) bool {
	if ordinaryClaudeRefreshToken(auth) == "" {
		return false
	}
	expiry, known := auth.OuterExpirationTime()
	return known && !expiry.After(time.Now())
}

func (e *ClaudeExecutor) PrepareRequestAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if !e.ShouldPrepareRequestAuth(auth) {
		return auth, nil
	}
	return e.Refresh(ctx, auth)
}

func (e *ClaudeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if auth == nil {
		return nil, statusErr{code: http.StatusBadRequest, msg: "missing Claude credential", requestScoped: true}
	}
	if auth.Disabled || auth.Status == cliproxyauth.StatusDisabled || auth.AutoQuarantined || auth.RefreshDisabled() {
		return auth, nil
	}
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	token := ordinaryClaudeRefreshToken(auth)
	if token == "" {
		return auth, nil
	}

	client := *helps.NewProxyAwareHTTPClient(ctx, e.cfg, nil, 30*time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// Do not replay a refresh whose token may already have been rotated.
	td, err := claudeauth.NewClaudeAuthWithHTTPClient(&client).RefreshTokens(ctx, token)
	if err != nil {
		return nil, err
	}
	if td == nil || strings.TrimSpace(td.AccessToken) == "" {
		return nil, statusErr{code: http.StatusBadGateway, msg: "invalid Claude OAuth refresh response"}
	}
	refreshed := auth.Clone()
	claudeauth.EnsureMetadataMap(&refreshed.Metadata)
	claudeauth.StoreMetadataString(&refreshed.Metadata, "access_token", td.AccessToken)
	claudeauth.StoreMetadataString(&refreshed.Metadata, "refresh_token", td.RefreshToken)
	claudeauth.StoreMetadataString(&refreshed.Metadata, "expired", td.Expire)
	claudeauth.StoreMetadataString(&refreshed.Metadata, "email", td.Email)
	claudeauth.StoreMetadataString(&refreshed.Metadata, "account_uuid", td.AccountUUID)
	claudeauth.StoreMetadataString(&refreshed.Metadata, "organization_uuid", td.OrganizationUUID)
	claudeauth.StoreMetadataString(&refreshed.Metadata, "organization_name", td.OrganizationName)
	claudeauth.StoreMetadataValue(&refreshed.Metadata, "type", "claude")
	claudeauth.StoreMetadataValue(&refreshed.Metadata, "last_refresh", time.Now().UTC().Format(time.RFC3339))
	return refreshed, nil
}
