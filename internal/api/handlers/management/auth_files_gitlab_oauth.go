package management

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	gitlabauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/gitlab"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const gitLabWebProvider = "gitlab"

var gitLabFileNameChars = regexp.MustCompile(`[^a-z0-9_.-]+`)

var newGitLabWebClient = func(cfg *config.Config) *gitlabauth.AuthClient {
	return gitlabauth.NewAuthClientWithHTTPClient(ordinaryWebOAuthHTTPClient(cfg))
}

func ordinaryWebOAuthHTTPClient(cfg *config.Config) *http.Client {
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return util.SetProxy(&cfg.SDKConfig, client)
}

func (h *Handler) RequestGitLabToken(c *gin.Context) {
	cfg := h.configSnapshot()
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "OAuth configuration unavailable"})
		return
	}
	baseURL, err := configuredGitLabWebURL(c.Query("base_url"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	clientID := strings.TrimSpace(os.Getenv("GITLAB_OAUTH_CLIENT_ID"))
	requestedID := strings.TrimSpace(c.Query("client_id"))
	secret := strings.TrimSpace(os.Getenv("GITLAB_OAUTH_CLIENT_SECRET"))
	if c.Query("client_secret") != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "configure the OAuth client secret on the server"})
		return
	}
	if clientID == "" && secret == "" {
		clientID = requestedID
	}
	if clientID == "" || (requestedID != "" && requestedID != clientID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a matching GitLab OAuth client ID is required"})
		return
	}
	redirect, err := gitLabWebRedirect(cfg)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	pkce, err := gitlabauth.GeneratePKCECodes()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization challenge"})
		return
	}
	state, err := misc.GenerateRandomState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization state"})
		return
	}
	client := newGitLabWebClient(cfg)
	authURL, err := client.GenerateAuthURL(baseURL, clientID, redirect, state, pkce)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid GitLab OAuth application configuration"})
		return
	}
	if err := h.registerFileOAuthSession(state, gitLabWebProvider, cfg.AuthDir, ""); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "OAuth callback directory is unavailable"})
		return
	}
	go h.completeGitLabOAuth(cfg.AuthDir, state, baseURL, redirect, clientID, secret, pkce.CodeVerifier, client)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) completeGitLabOAuth(dir, state, baseURL, redirect, clientID, secret, verifier string, client *gitlabauth.AuthClient) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	go watchOAuthSessionCancel(ctx, cancel, state, gitLabWebProvider)
	name := filepath.Join(dir, ".oauth-gitlab-"+state+".oauth")
	defer os.Remove(name)
	cb, err := waitBuiltinOAuthCallback(ctx, name, state, gitLabWebProvider)
	if err != nil || cb == nil || cb.State != state || cb.Error != "" || cb.Code == "" {
		if IsOAuthSessionPending(state, gitLabWebProvider) {
			SetOAuthSessionError(state, "GitLab authorization did not complete")
		}
		return
	}
	token, err := client.ExchangeCodeForTokens(ctx, baseURL, clientID, secret, redirect, cb.Code, verifier)
	if err != nil || token == nil || strings.TrimSpace(token.AccessToken) == "" {
		SetOAuthSessionError(state, "GitLab authorization code exchange failed")
		return
	}
	user, err := client.GetCurrentUser(ctx, baseURL, token.AccessToken)
	record := gitLabWebRecord(baseURL, "oauth", user)
	if err != nil || record == nil {
		SetOAuthSessionError(state, "GitLab account lookup failed")
		return
	}
	record.Metadata["access_token"] = token.AccessToken
	record.Metadata["refresh_token"] = token.RefreshToken
	record.Metadata["scope"] = token.Scope
	record.Metadata["token_type"] = token.TokenType
	record.Metadata["oauth_client_id"] = clientID
	if secret != "" {
		record.Metadata["oauth_client_secret"] = secret
	}
	if expires := gitlabauth.TokenExpiry(time.Now(), token); !expires.IsZero() {
		record.Metadata["oauth_expires_at"] = expires.Format(time.RFC3339)
	}
	h.commitAndCompleteBuiltinOAuthSession(ctx, state, gitLabWebProvider, "", record, "Failed to save GitLab credentials", true)
}

func (h *Handler) RequestGitLabPATToken(c *gin.Context) {
	var body struct {
		BaseURL string `json:"base_url"`
		Token   string `json:"personal_access_token"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Token) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "personal_access_token is required"})
		return
	}
	baseURL, err := configuredGitLabWebURL(body.BaseURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.mu.Lock()
	cfg := h.cfg.CloneForRuntime()
	scope := oauthFileScope{owner: h, generation: h.fileOAuthGeneration}
	h.mu.Unlock()
	dir, err := fileOAuthDirectory(fileOAuthConfigDir(cfg))
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "credential configuration unavailable"})
		return
	}

	ctx := c.Request.Context()
	client := newGitLabWebClient(cfg)
	token := strings.TrimSpace(body.Token)
	user, err := client.GetCurrentUser(ctx, baseURL, token)
	record := gitLabWebRecord(baseURL, "pat", user)
	if err != nil || record == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "GitLab account lookup failed"})
		return
	}
	pat, err := client.GetPersonalAccessTokenSelf(ctx, baseURL, token)
	if err != nil || pat == nil || pat.ID == 0 || pat.UserID != user.ID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "GitLab personal access token could not be verified"})
		return
	}
	record.Metadata["personal_access_token"] = token
	record.Metadata["pat_id"] = pat.ID
	record.Metadata["pat_name"] = pat.Name
	record.Metadata["pat_scopes"] = pat.Scopes
	h.mu.Lock()
	currentDir, err := fileOAuthDirectory(fileOAuthConfigDir(h.cfg))
	stillCurrent := err == nil && currentDir == dir && scope.generation == h.fileOAuthGeneration
	h.mu.Unlock()
	if !stillCurrent {
		c.JSON(http.StatusConflict, gin.H{"error": "credential configuration changed"})
		return
	}
	cfg.AuthDir = dir
	if _, err := h.saveTokenRecordWithConfig(ctx, record, cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save GitLab credentials"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "file_name": record.FileName, "username": strings.TrimSpace(user.Username)})
}

func configuredGitLabWebURL(requested string) (string, error) {
	configured := gitlabauth.NormalizeBaseURL(os.Getenv("GITLAB_BASE_URL"))
	endpoint, err := url.Parse(configured)
	if err != nil || !strings.EqualFold(endpoint.Scheme, "https") || endpoint.Hostname() == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.ForceQuery {
		return "", fmt.Errorf("invalid configured GitLab HTTPS URL")
	}
	if strings.TrimSpace(requested) != "" && gitlabauth.NormalizeBaseURL(requested) != configured {
		return "", fmt.Errorf("the requested GitLab URL does not match the configured instance")
	}
	return configured, nil
}

func gitLabWebRedirect(cfg *config.Config) (string, error) {
	if raw := strings.TrimSpace(os.Getenv("GITLAB_OAUTH_REDIRECT_URI")); raw != "" {
		redirect, err := url.Parse(raw)
		if err != nil || redirect.Hostname() == "" || redirect.Path != "/gitlab/callback" ||
			redirect.User != nil || redirect.RawQuery != "" || redirect.Fragment != "" || redirect.ForceQuery {
			return "", fmt.Errorf("invalid configured GitLab callback URL")
		}
		host := net.ParseIP(redirect.Hostname())
		if redirect.Scheme != "https" && (redirect.Scheme != "http" || host == nil || !host.IsLoopback()) {
			return "", fmt.Errorf("GitLab callback requires HTTPS or a loopback HTTP URL")
		}
		return raw, nil
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return "", fmt.Errorf("callback server unavailable")
	}
	scheme := "http"
	if cfg.TLS.Enable {
		scheme = "https"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d/gitlab/callback", scheme, cfg.Port), nil
}

func gitLabWebRecord(baseURL, mode string, user *gitlabauth.User) *coreauth.Auth {
	if user == nil || strings.TrimSpace(user.Username) == "" {
		return nil
	}
	username := strings.TrimSpace(user.Username)
	account := strings.Trim(gitLabFileNameChars.ReplaceAllString(strings.ToLower(username), "-"), "-")
	if account == "" {
		return nil
	}
	kind, suffix := "oauth", ""
	if mode == "pat" {
		kind, suffix = "personal_access_token", "-pat"
	}
	name := "gitlab-" + account + suffix + ".json"
	metadata := map[string]any{
		"type": gitLabWebProvider, "base_url": baseURL, "auth_method": mode, "auth_kind": kind,
		"username": username, "name": strings.TrimSpace(user.Name), "last_refresh": time.Now().UTC().Format(time.RFC3339),
	}
	if email := strings.TrimSpace(user.Email); email != "" {
		metadata["email"] = email
	} else if email := strings.TrimSpace(user.PublicEmail); email != "" {
		metadata["email"] = email
	}
	return &coreauth.Auth{ID: name, FileName: name, Label: username, Provider: gitLabWebProvider, Metadata: metadata}
}
