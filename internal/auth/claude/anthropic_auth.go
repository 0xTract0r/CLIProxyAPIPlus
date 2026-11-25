// Package claude provides OAuth2 authentication functionality for Anthropic's Claude API.
// This package implements the complete OAuth2 flow with PKCE (Proof Key for Code Exchange)
// for secure authentication with Claude API, including token exchange, refresh, and storage.
package claude

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

// OAuth configuration constants for Claude/Anthropic
const (
	AuthURL = "https://claude.ai/oauth/authorize"
	// TokenURL is the authorization-code exchange endpoint. Claude Code 2.1.220
	// posts the code exchange to platform.claude.com, not api.anthropic.com.
	TokenURL         = "https://platform.claude.com/v1/oauth/token"
	RefreshTokenURL  = "https://platform.claude.com/v1/oauth/token"
	ProfileURL       = "https://api.anthropic.com/api/oauth/profile"
	RolesURL         = "https://api.anthropic.com/api/oauth/claude_cli/roles"
	ClientID         = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	RedirectURI      = "http://localhost:54545/callback"
	ClaudeOAuthScope = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"

	// Native Claude Code includes the Console scope when authorizing a new login.
	// Keep the established token refresh scope unchanged.
	claudeOAuthAuthorizeScope = "org:create_api_key " + ClaudeOAuthScope

	claudeRefreshMinBackoff = 5 * time.Second
	claudeRefreshMaxBackoff = 5 * time.Minute
	claudeRefreshTimeout    = 30 * time.Second
)

var (
	claudeRefreshGroup singleflight.Group
	claudeRefreshMu    sync.Mutex
	claudeRefreshBlock = make(map[string]time.Time)
)

type refreshHTTPError struct {
	status    int
	message   string
	retryable bool
}

func (e *refreshHTTPError) Error() string {
	return fmt.Sprintf("token refresh failed with status %d: %s", e.status, e.message)
}

func (e *refreshHTTPError) Retryable() bool {
	return e != nil && e.retryable
}

func resetClaudeRefreshState() {
	claudeRefreshMu.Lock()
	defer claudeRefreshMu.Unlock()
	claudeRefreshBlock = make(map[string]time.Time)
	claudeRefreshGroup = singleflight.Group{}
}

func claudeRefreshBlockedUntil(refreshToken string) time.Time {
	claudeRefreshMu.Lock()
	defer claudeRefreshMu.Unlock()
	return claudeRefreshBlock[refreshToken]
}

func setClaudeRefreshBlockedUntil(refreshToken string, until time.Time) {
	claudeRefreshMu.Lock()
	defer claudeRefreshMu.Unlock()
	claudeRefreshBlock[refreshToken] = until
}

func clearClaudeRefreshBlockedUntil(refreshToken string) {
	claudeRefreshMu.Lock()
	defer claudeRefreshMu.Unlock()
	delete(claudeRefreshBlock, refreshToken)
}

func clampClaudeRefreshBackoff(d time.Duration) time.Duration {
	if d < claudeRefreshMinBackoff {
		return claudeRefreshMinBackoff
	}
	if d > claudeRefreshMaxBackoff {
		return claudeRefreshMaxBackoff
	}
	return d
}

func parseClaudeRetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return claudeRefreshMinBackoff
	}
	if raw := strings.TrimSpace(resp.Header.Get("Retry-After")); raw != "" {
		if seconds, err := time.ParseDuration(raw + "s"); err == nil {
			return clampClaudeRefreshBackoff(seconds)
		}
		if when, err := http.ParseTime(raw); err == nil {
			return clampClaudeRefreshBackoff(time.Until(when))
		}
	}
	if raw := strings.TrimSpace(resp.Header.Get("Retry-After-Ms")); raw != "" {
		if ms, err := time.ParseDuration(raw + "ms"); err == nil {
			return clampClaudeRefreshBackoff(ms)
		}
	}
	return claudeRefreshMinBackoff
}

func isClaudeRefreshRetryable(err error) bool {
	var httpErr *refreshHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Retryable()
	}
	// A transport or response-decoding error is ambiguous: Anthropic may have
	// consumed the single-use refresh token even though the response was lost.
	// Retrying the same token can turn a transient network fault into
	// invalid_grant. Only explicit retryable HTTP responses are safe to replay.
	return false
}

// tokenResponse represents the response structure from Anthropic's OAuth token endpoint.
// It contains access token, refresh token, and associated user/organization information.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
	Account struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account"`
}

// authorizationCodeExchangeRequest is the authorization-code exchange body.
type authorizationCodeExchangeRequest struct {
	GrantType    string `json:"grant_type"`
	Code         string `json:"code"`
	RedirectURI  string `json:"redirect_uri"`
	ClientID     string `json:"client_id"`
	CodeVerifier string `json:"code_verifier"`
	State        string `json:"state"`
}

// OAuthProfile is the account identity returned by Anthropic's OAuth profile endpoint.
type OAuthProfile struct {
	Account struct {
		UUID  string `json:"uuid"`
		Email string `json:"email"`
	} `json:"account"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
}

// ClaudeAuth handles Anthropic OAuth2 authentication flow.
// It provides methods for generating authorization URLs, exchanging codes for tokens,
// and refreshing expired tokens using PKCE for enhanced security.
type ClaudeAuth struct {
	httpClient     *http.Client
	authorizeScope string
}

func resolveClaudeAuthorizeScope(cfg *config.Config) string {
	if cfg != nil && !cfg.Claude.OAuthIncludeCreateAPIKeyEnabled() {
		return ClaudeOAuthScope
	}
	return claudeOAuthAuthorizeScope
}

// NewClaudeAuth creates a new Anthropic authentication service.
//
// Parameters:
//   - cfg: The application configuration containing proxy settings
//
// Returns:
//   - *ClaudeAuth: A new Claude authentication service instance
func NewClaudeAuth(cfg *config.Config) *ClaudeAuth {
	return NewClaudeAuthWithProxyURL(cfg, "")
}

// NewClaudeAuthWithProxyURL creates a new Anthropic authentication service with a proxy override.
// proxyURL takes precedence over cfg.ProxyURL when non-empty.
func NewClaudeAuthWithProxyURL(cfg *config.Config, proxyURL string) *ClaudeAuth {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	return &ClaudeAuth{httpClient: newClaudeOAuthHTTPClient(proxyURL), authorizeScope: resolveClaudeAuthorizeScope(cfg)}
}

func NewClaudeAuthWithHTTPClient(client *http.Client) *ClaudeAuth {
	if client == nil {
		return NewClaudeAuth(nil)
	}
	return &ClaudeAuth{httpClient: client}
}

func setClaudeOAuthJSONHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	if req.Method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
}

func (o *ClaudeAuth) fetchOAuthControlPlaneJSON(ctx context.Context, endpoint, accessToken, label string) ([]byte, error) {
	if o == nil || o.httpClient == nil {
		return nil, fmt.Errorf("fetch Claude OAuth %s: HTTP client is nil", label)
	}
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, fmt.Errorf("fetch Claude OAuth %s: access token is empty", label)
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errRequest != nil {
		return nil, fmt.Errorf("create Claude OAuth %s request: %w", label, errRequest)
	}
	setClaudeOAuthJSONHeaders(req)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Cache-Control", "no-cache")

	resp, errDo := o.httpClient.Do(req)
	if errDo != nil {
		switch {
		case errors.Is(errDo, context.Canceled):
			return nil, context.Canceled
		case errors.Is(errDo, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		case errors.Is(errDo, errInvalidClaudeOAuthProxy):
			return nil, errInvalidClaudeOAuthProxy
		default:
			return nil, fmt.Errorf("Claude OAuth %s request failed", label)
		}
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("failed to close Claude OAuth %s response body: %v", label, errClose)
		}
	}()
	body, errRead := readClaudeOAuthResponseBody(resp)
	if errRead != nil {
		return nil, fmt.Errorf("read Claude OAuth %s response: %w", label, errRead)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("fetch Claude OAuth %s failed with status %d", label, resp.StatusCode)
	}
	return body, nil
}

// FetchOAuthProfile retrieves the account identity associated with an OAuth access token.
func (o *ClaudeAuth) FetchOAuthProfile(ctx context.Context, accessToken string) (*OAuthProfile, error) {
	body, errFetch := o.fetchOAuthControlPlaneJSON(ctx, ProfileURL, accessToken, "profile")
	if errFetch != nil {
		return nil, errFetch
	}
	var profile OAuthProfile
	if errUnmarshal := json.Unmarshal(body, &profile); errUnmarshal != nil {
		return nil, errors.New("failed to parse Claude OAuth profile response")
	}
	if strings.TrimSpace(profile.Account.UUID) == "" {
		return nil, fmt.Errorf("fetch Claude OAuth profile: response account UUID is empty")
	}
	return &profile, nil
}

// FetchOAuthRoles retrieves the current OAuth account roles on explicit request.
func (o *ClaudeAuth) FetchOAuthRoles(ctx context.Context, accessToken string) (json.RawMessage, error) {
	body, err := o.fetchOAuthControlPlaneJSON(ctx, RolesURL, accessToken, "roles")
	if err != nil {
		return nil, err
	}
	if !json.Valid(body) {
		return nil, errors.New("failed to parse Claude OAuth roles response")
	}
	return json.RawMessage(body), nil
}

// GenerateAuthURL creates the OAuth authorization URL with PKCE.
// This method generates a secure authorization URL including PKCE challenge codes
// for the OAuth2 flow with Anthropic's API.
//
// Parameters:
//   - state: A random state parameter for CSRF protection
//   - pkceCodes: The PKCE codes for secure code exchange
//
// Returns:
//   - string: The complete authorization URL
//   - string: The state parameter for verification
//   - error: An error if PKCE codes are missing or URL generation fails
func (o *ClaudeAuth) GenerateAuthURL(state string, pkceCodes *PKCECodes) (string, string, error) {
	if pkceCodes == nil {
		return "", "", fmt.Errorf("PKCE codes are required")
	}

	scope := claudeOAuthAuthorizeScope
	if o != nil && o.authorizeScope != "" {
		scope = o.authorizeScope
	}

	params := url.Values{
		"code":                  {"true"},
		"client_id":             {ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {RedirectURI},
		"scope":                 {scope},
		"code_challenge":        {pkceCodes.CodeChallenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}

	authURL := fmt.Sprintf("%s?%s", AuthURL, params.Encode())
	return authURL, state, nil
}

// parseCodeAndState extracts the authorization code and state from the callback response.
// It handles the parsing of the code parameter which may contain additional fragments.
//
// Parameters:
//   - code: The raw code parameter from the OAuth callback
//
// Returns:
//   - parsedCode: The extracted authorization code
//   - parsedState: The extracted state parameter if present
func (c *ClaudeAuth) parseCodeAndState(code string) (parsedCode, parsedState string) {
	splits := strings.Split(code, "#")
	parsedCode = splits[0]
	if len(splits) > 1 {
		parsedState = splits[1]
	}
	return
}

// ExchangeCodeForTokens exchanges authorization code for access tokens.
// This method implements the OAuth2 token exchange flow using PKCE for security.
// It sends the authorization code along with PKCE verifier to get access and refresh tokens.
//
// Parameters:
//   - ctx: The context for the request
//   - code: The authorization code received from OAuth callback
//   - state: The state parameter for verification
//   - pkceCodes: The PKCE codes for secure verification
//
// Returns:
//   - *ClaudeAuthBundle: The complete authentication bundle with tokens
//   - error: An error if token exchange fails
func (o *ClaudeAuth) ExchangeCodeForTokens(ctx context.Context, code, state string, pkceCodes *PKCECodes) (*ClaudeAuthBundle, error) {
	if pkceCodes == nil {
		return nil, fmt.Errorf("PKCE codes are required for token exchange")
	}
	newCode, newState := o.parseCodeAndState(code)

	if strings.TrimSpace(newCode) == "" || strings.TrimSpace(state) == "" || strings.TrimSpace(pkceCodes.CodeVerifier) == "" {
		return nil, errors.New("authorization code, expected state and PKCE verifier are required")
	}
	if newState != "" && newState != state {
		return nil, errors.New("Claude OAuth state mismatch")
	}

	reqBody := authorizationCodeExchangeRequest{
		GrantType:    "authorization_code",
		Code:         newCode,
		RedirectURI:  RedirectURI,
		ClientID:     ClientID,
		CodeVerifier: pkceCodes.CodeVerifier,
		State:        state,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, fmt.Errorf("failed to create token request: %w", err)
	}
	setClaudeOAuthJSONHeaders(req)

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("failed to close response body: %v", errClose)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed with status %d", resp.StatusCode)
	}

	body, err := readClaudeOAuthResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read token response: %w", err)
	}

	var tokenResp tokenResponse
	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, errors.New("failed to parse Claude OAuth token response")
	}
	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return nil, errors.New("Claude OAuth token response is missing an access token")
	}

	// Create token data.
	tokenData := ClaudeTokenData{
		AccessToken:      tokenResp.AccessToken,
		RefreshToken:     tokenResp.RefreshToken,
		Email:            tokenResp.Account.EmailAddress,
		AccountUUID:      tokenResp.Account.UUID,
		OrganizationUUID: tokenResp.Organization.UUID,
		OrganizationName: tokenResp.Organization.Name,
		Expire:           time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}

	// Create auth bundle.
	bundle := &ClaudeAuthBundle{
		TokenData:   tokenData,
		LastRefresh: time.Now().Format(time.RFC3339),
	}

	return bundle, nil
}

// RefreshTokens refreshes the access token using the refresh token.
// This method exchanges a valid refresh token for a new access token,
// extending the user's authenticated session.
//
// Parameters:
//   - ctx: The context for the request
//   - refreshToken: The refresh token to use for getting new access token
//
// Returns:
//   - *ClaudeTokenData: The new token data with updated access token
//   - error: An error if token refresh fails
func (o *ClaudeAuth) RefreshTokens(ctx context.Context, refreshToken string) (*ClaudeTokenData, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if blockedUntil := claudeRefreshBlockedUntil(refreshToken); blockedUntil.After(time.Now()) {
		return nil, &refreshHTTPError{
			status:    http.StatusTooManyRequests,
			message:   fmt.Sprintf("refresh temporarily blocked until %s", blockedUntil.Format(time.RFC3339)),
			retryable: false,
		}
	}

	result, err, _ := claudeRefreshGroup.Do(refreshToken, func() (interface{}, error) {
		refreshCtx, cancelRefresh := context.WithTimeout(context.WithoutCancel(ctx), claudeRefreshTimeout)
		defer cancelRefresh()
		if errReadiness := claudeRefreshReadiness(refreshCtx); errReadiness != nil {
			return nil, errReadiness
		}

		return o.refreshTokensSingleFlight(refreshCtx, refreshToken)
	})
	if err != nil {
		return nil, err
	}
	tokenData, ok := result.(*ClaudeTokenData)
	if !ok || tokenData == nil {
		return nil, fmt.Errorf("token refresh failed: invalid single-flight result")
	}
	return tokenData, nil
}

func (o *ClaudeAuth) refreshTokensSingleFlight(ctx context.Context, refreshToken string) (*ClaudeTokenData, error) {
	if blockedUntil := claudeRefreshBlockedUntil(refreshToken); blockedUntil.After(time.Now()) {
		return nil, &refreshHTTPError{
			status:    http.StatusTooManyRequests,
			message:   fmt.Sprintf("refresh temporarily blocked until %s", blockedUntil.Format(time.RFC3339)),
			retryable: false,
		}
	}

	reqBody := map[string]interface{}{
		"client_id":     ClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"scope":         ClaudeOAuthScope,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", RefreshTokenURL, strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, fmt.Errorf("failed to create refresh request: %w", err)
	}
	setClaudeOAuthJSONHeaders(req)

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token refresh request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		message := http.StatusText(resp.StatusCode)
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := parseClaudeRetryAfter(resp)
			setClaudeRefreshBlockedUntil(refreshToken, time.Now().Add(retryAfter))
			return nil, &refreshHTTPError{status: resp.StatusCode, message: message, retryable: false}
		}
		return nil, &refreshHTTPError{
			status:    resp.StatusCode,
			message:   message,
			retryable: resp.StatusCode >= http.StatusInternalServerError,
		}
	}

	body, err := readClaudeOAuthResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read refresh response: %w", err)
	}

	var tokenResp tokenResponse
	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, errors.New("failed to parse Claude OAuth token response")
	}
	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return nil, errors.New("Claude OAuth token response is missing an access token")
	}

	clearClaudeRefreshBlockedUntil(refreshToken)
	if strings.TrimSpace(tokenResp.RefreshToken) == "" {
		tokenResp.RefreshToken = refreshToken
	}
	tokenData := &ClaudeTokenData{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		Expire:       time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}
	tokenData.Email = tokenResp.Account.EmailAddress
	tokenData.AccountUUID = tokenResp.Account.UUID
	tokenData.OrganizationUUID = tokenResp.Organization.UUID
	tokenData.OrganizationName = tokenResp.Organization.Name
	return tokenData, nil
}

// CreateTokenStorage creates a new ClaudeTokenStorage from auth bundle and user info.
// This method converts the authentication bundle into a token storage structure
// suitable for persistence and later use.
//
// Parameters:
//   - bundle: The authentication bundle containing token data
//
// Returns:
//   - *ClaudeTokenStorage: A new token storage instance
func (o *ClaudeAuth) CreateTokenStorage(bundle *ClaudeAuthBundle) *ClaudeTokenStorage {
	storage := &ClaudeTokenStorage{
		AccessToken:      bundle.TokenData.AccessToken,
		RefreshToken:     bundle.TokenData.RefreshToken,
		LastRefresh:      bundle.LastRefresh,
		Email:            bundle.TokenData.Email,
		AccountUUID:      bundle.TokenData.AccountUUID,
		OrganizationUUID: bundle.TokenData.OrganizationUUID,
		OrganizationName: bundle.TokenData.OrganizationName,
		Expire:           bundle.TokenData.Expire,
	}

	return storage
}

// RefreshTokensWithRetry refreshes tokens with automatic retry logic.
// This method implements exponential backoff retry logic for token refresh operations,
// providing resilience against temporary network or service issues.
//
// Parameters:
//   - ctx: The context for the request
//   - refreshToken: The refresh token to use
//   - maxRetries: The maximum number of retry attempts
//
// Returns:
//   - *ClaudeTokenData: The refreshed token data
//   - error: An error if all retry attempts fail
func (o *ClaudeAuth) RefreshTokensWithRetry(ctx context.Context, refreshToken string, maxRetries int) (*ClaudeTokenData, error) {
	if maxRetries <= 0 {
		maxRetries = 1
	}
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Wait before retry
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}

		tokenData, err := o.RefreshTokens(ctx, refreshToken)
		if err == nil {
			return tokenData, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrClaudeRefreshDeferred) {
			return nil, err
		}

		lastErr = err
		log.Warnf("Token refresh attempt %d failed: %v", attempt+1, err)
		if !isClaudeRefreshRetryable(err) {
			break
		}
	}

	return nil, fmt.Errorf("token refresh failed after %d attempts: %w", maxRetries, lastErr)
}

// UpdateTokenStorage updates an existing token storage with new token data.
// This method refreshes the token storage with newly obtained access and refresh tokens,
// updating timestamps and expiration information.
//
// Parameters:
//   - storage: The existing token storage to update
//   - tokenData: The new token data to apply
func (o *ClaudeAuth) UpdateTokenStorage(storage *ClaudeTokenStorage, tokenData *ClaudeTokenData) {
	storage.AccessToken = tokenData.AccessToken
	storage.RefreshToken = tokenData.RefreshToken
	storage.LastRefresh = time.Now().Format(time.RFC3339)
	if strings.TrimSpace(tokenData.Email) != "" {
		storage.Email = tokenData.Email
	}
	if strings.TrimSpace(tokenData.AccountUUID) != "" {
		storage.AccountUUID = tokenData.AccountUUID
	}
	if strings.TrimSpace(tokenData.OrganizationUUID) != "" {
		storage.OrganizationUUID = tokenData.OrganizationUUID
	}
	if strings.TrimSpace(tokenData.OrganizationName) != "" {
		storage.OrganizationName = tokenData.OrganizationName
	}
	storage.Expire = tokenData.Expire
}
