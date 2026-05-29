// Package codex provides authentication and token management for OpenAI's Codex API.
// It handles the OAuth2 flow, including generating authorization URLs, exchanging
// authorization codes for tokens, and refreshing expired tokens. The package also
// defines data structures for storing and managing Codex authentication credentials.
package codex

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

// OAuth configuration constants for OpenAI Codex
const (
	AuthURL     = "https://auth.openai.com/oauth/authorize"
	TokenURL    = "https://auth.openai.com/oauth/token"
	ClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	RedirectURI = "http://localhost:1455/auth/callback"
)

// CodexAuth handles the OpenAI OAuth2 authentication flow.
// It manages the HTTP client and provides methods for generating authorization URLs,
// exchanging authorization codes for tokens, and refreshing access tokens.
type CodexAuth struct {
	httpClient *http.Client
}

// NewCodexAuth creates a new CodexAuth service instance.
// It initializes an HTTP client with proxy settings from the provided configuration.
func NewCodexAuth(cfg *config.Config) *CodexAuth {
	return NewCodexAuthWithProxyURL(cfg, "")
}

// NewCodexAuthWithProxyURL creates a new CodexAuth service instance.
// proxyURL takes precedence over cfg.ProxyURL when non-empty.
func NewCodexAuthWithProxyURL(cfg *config.Config, proxyURL string) *CodexAuth {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	return &CodexAuth{
		httpClient: util.SetProxy(&sdkCfg, &http.Client{}),
	}
}

// NewCodexAuthWithHTTPClient creates a CodexAuth with a caller-managed HTTP
// client. Management OAuth uses this to bind token exchange to the same
// account-scoped runtime transport and managed headers that will be persisted.
func NewCodexAuthWithHTTPClient(client *http.Client) *CodexAuth {
	if client == nil {
		client = util.SetProxy(nil, &http.Client{})
	}
	return &CodexAuth{httpClient: client}
}

// GenerateAuthURL creates the OAuth authorization URL with PKCE (Proof Key for Code Exchange).
// It constructs the URL with the necessary parameters, including the client ID,
// response type, redirect URI, scopes, and PKCE challenge.
func (o *CodexAuth) GenerateAuthURL(state string, pkceCodes *PKCECodes) (string, error) {
	if pkceCodes == nil {
		return "", fmt.Errorf("PKCE codes are required")
	}

	params := url.Values{
		"client_id":                  {ClientID},
		"response_type":              {"code"},
		"redirect_uri":               {RedirectURI},
		"scope":                      {"openid email profile offline_access"},
		"state":                      {state},
		"code_challenge":             {pkceCodes.CodeChallenge},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}

	authURL := fmt.Sprintf("%s?%s", AuthURL, params.Encode())
	return authURL, nil
}

// ExchangeCodeForTokens exchanges an authorization code for access and refresh tokens.
// It performs an HTTP POST request to the OpenAI token endpoint with the provided
// authorization code and PKCE verifier.
func (o *CodexAuth) ExchangeCodeForTokens(ctx context.Context, code string, pkceCodes *PKCECodes) (*CodexAuthBundle, error) {
	return o.ExchangeCodeForTokensWithRedirect(ctx, code, RedirectURI, pkceCodes)
}

// ExchangeCodeForTokensWithRedirect exchanges an authorization code for tokens using
// a caller-provided redirect URI. This supports alternate auth flows such as device
// login while preserving the existing token parsing and storage behavior.
func (o *CodexAuth) ExchangeCodeForTokensWithRedirect(ctx context.Context, code, redirectURI string, pkceCodes *PKCECodes) (*CodexAuthBundle, error) {
	if pkceCodes == nil {
		return nil, fmt.Errorf("PKCE codes are required for token exchange")
	}
	if strings.TrimSpace(redirectURI) == "" {
		return nil, fmt.Errorf("redirect URI is required for token exchange")
	}

	// Prepare token exchange request
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code":          {code},
		"redirect_uri":  {strings.TrimSpace(redirectURI)},
		"code_verifier": {pkceCodes.CodeVerifier},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create token request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := readTokenResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read token response: %w", err)
	}
	// log.Debugf("Token response: %s", string(body))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed: %s", summarizeTokenHTTPResponse(resp, body))
	}

	// Parse token response
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}

	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w; %s", err, summarizeTokenHTTPResponse(resp, body))
	}

	// Extract account ID from ID token
	claims, err := ParseJWTToken(tokenResp.IDToken)
	if err != nil {
		log.Warnf("Failed to parse ID token: %v", err)
	}

	accountID := ""
	email := ""
	if claims != nil {
		accountID = claims.GetAccountID()
		email = claims.GetUserEmail()
	}

	// Create token data
	tokenData := CodexTokenData{
		IDToken:      tokenResp.IDToken,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		AccountID:    accountID,
		Email:        email,
		Expire:       time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}

	// Create auth bundle
	bundle := &CodexAuthBundle{
		TokenData:   tokenData,
		LastRefresh: time.Now().Format(time.RFC3339),
	}

	return bundle, nil
}

// RefreshTokens refreshes an access token using a refresh token.
// This method is called when an access token has expired. It makes a request to the
// token endpoint to obtain a new set of tokens.
func (o *CodexAuth) RefreshTokens(ctx context.Context, refreshToken string) (*CodexTokenData, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token is required")
	}

	data := url.Values{
		"client_id":     {ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {"openid profile email"},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create refresh request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token refresh request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := readTokenResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token refresh failed: %s", summarizeTokenHTTPResponse(resp, body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}

	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse refresh response: %w; %s", err, summarizeTokenHTTPResponse(resp, body))
	}

	// Extract account ID from ID token
	claims, err := ParseJWTToken(tokenResp.IDToken)
	if err != nil {
		log.Warnf("Failed to parse refreshed ID token: %v", err)
	}

	accountID := ""
	email := ""
	if claims != nil {
		accountID = claims.GetAccountID()
		email = claims.Email
	}

	return &CodexTokenData{
		IDToken:      tokenResp.IDToken,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		AccountID:    accountID,
		Email:        email,
		Expire:       time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}

// CreateTokenStorage creates a new CodexTokenStorage from a CodexAuthBundle.
// It populates the storage struct with token data, user information, and timestamps.
func (o *CodexAuth) CreateTokenStorage(bundle *CodexAuthBundle) *CodexTokenStorage {
	storage := &CodexTokenStorage{
		IDToken:      bundle.TokenData.IDToken,
		AccessToken:  bundle.TokenData.AccessToken,
		RefreshToken: bundle.TokenData.RefreshToken,
		AccountID:    bundle.TokenData.AccountID,
		LastRefresh:  bundle.LastRefresh,
		Email:        bundle.TokenData.Email,
		Expire:       bundle.TokenData.Expire,
	}

	return storage
}

// RefreshTokensWithRetry refreshes tokens with a built-in retry mechanism.
// It attempts to refresh the tokens up to a specified maximum number of retries,
// with an exponential backoff strategy to handle transient network errors.
func (o *CodexAuth) RefreshTokensWithRetry(ctx context.Context, refreshToken string, maxRetries int) (*CodexTokenData, error) {
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
		if isNonRetryableRefreshErr(err) {
			log.Errorf("Token refresh attempt %d failed with non-retryable error: %v", attempt+1, err)
			return nil, err
		}

		lastErr = err
		if attempt+1 >= maxRetries {
			log.Errorf("Token refresh attempt %d failed: %v", attempt+1, err)
		} else {
			log.Warnf("Token refresh attempt %d failed: %v", attempt+1, err)
		}
	}

	return nil, fmt.Errorf("token refresh failed after %d attempts: %w", maxRetries, lastErr)
}

func isNonRetryableRefreshErr(err error) bool {
	if err == nil {
		return false
	}
	raw := strings.ToLower(err.Error())
	if raw == "" {
		return false
	}
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(raw)
	if strings.Contains(normalized, "refresh_token_reused") || strings.Contains(normalized, "refresh_token_reuse") || strings.Contains(normalized, "refresh_token_already_used") {
		return true
	}
	return strings.Contains(normalized, "refresh") && strings.Contains(normalized, "already") && (strings.Contains(normalized, "used") || strings.Contains(normalized, "reuse"))
}

func summarizeTokenHTTPResponse(resp *http.Response, body []byte) string {
	status := 0
	contentType := ""
	contentEncoding := ""
	if resp != nil {
		status = resp.StatusCode
		contentType = strings.TrimSpace(resp.Header.Get("Content-Type"))
		contentEncoding = strings.TrimSpace(resp.Header.Get("Content-Encoding"))
	}
	if contentType == "" {
		contentType = "<empty>"
	}
	if contentEncoding == "" {
		contentEncoding = "<empty>"
	}

	preview := strings.TrimSpace(string(body))
	const maxPreview = 512
	if len(preview) > maxPreview {
		preview = preview[:maxPreview] + "...(truncated)"
	}
	if preview == "" {
		preview = "<empty>"
	} else {
		preview = strconv.QuoteToASCII(preview)
	}
	return fmt.Sprintf("status=%d content_type=%s content_encoding=%s body_preview=%s", status, contentType, contentEncoding, preview)
}

func readTokenResponseBody(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("empty token response body")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	encodings := tokenContentEncodings(resp.Header.Get("Content-Encoding"))
	if len(encodings) == 0 {
		return body, nil
	}

	decoded := body
	for i := len(encodings) - 1; i >= 0; i-- {
		decoded, err = decodeTokenResponseBody(decoded, encodings[i])
		if err != nil {
			return nil, err
		}
	}
	return decoded, nil
}

func tokenContentEncodings(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	encodings := make([]string, 0, len(parts))
	for _, part := range parts {
		encoding := strings.ToLower(strings.TrimSpace(part))
		if encoding == "" || encoding == "identity" {
			continue
		}
		encodings = append(encodings, encoding)
	}
	return encodings
}

func decodeTokenResponseBody(body []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip", "x-gzip":
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("decode gzip token response: %w", err)
		}
		defer func() {
			_ = reader.Close()
		}()
		return io.ReadAll(reader)
	case "br":
		return io.ReadAll(brotli.NewReader(bytes.NewReader(body)))
	case "zstd":
		reader, err := zstd.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("decode zstd token response: %w", err)
		}
		defer reader.Close()
		return io.ReadAll(reader)
	case "deflate":
		reader, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("decode deflate token response: %w", err)
		}
		defer func() {
			_ = reader.Close()
		}()
		return io.ReadAll(reader)
	default:
		return nil, fmt.Errorf("unsupported token response content-encoding %q", encoding)
	}
}

// UpdateTokenStorage updates an existing CodexTokenStorage with new token data.
// This is typically called after a successful token refresh to persist the new credentials.
func (o *CodexAuth) UpdateTokenStorage(storage *CodexTokenStorage, tokenData *CodexTokenData) {
	storage.IDToken = tokenData.IDToken
	storage.AccessToken = tokenData.AccessToken
	storage.RefreshToken = tokenData.RefreshToken
	storage.AccountID = tokenData.AccountID
	storage.LastRefresh = time.Now().Format(time.RFC3339)
	storage.Email = tokenData.Email
	storage.Expire = tokenData.Expire
}
