package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/gitlab"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const GitLabCodeSuggestionsModel = "gitlab-code-suggestions"

const maxGitLabSuggestionResponse = 4 << 20

type GitLabExecutor struct{ cfg *config.Config }

func NewGitLabExecutor(cfg *config.Config) *GitLabExecutor { return &GitLabExecutor{cfg: cfg} }

func (e *GitLabExecutor) Identifier() string { return "gitlab" }

func (e *GitLabExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	base, err := gitLabRuntimeBaseURL(auth)
	if err != nil {
		return err
	}
	if req == nil || req.URL == nil || req.Method != http.MethodPost ||
		req.URL.String() != base+"/api/v4/code_suggestions/completions" || req.Host != "" && req.Host != req.URL.Host {
		return ordinaryProviderStop(http.StatusBadRequest, "GitLab credentials are scoped to the configured Code Suggestions endpoint")
	}
	token := gitLabRuntimeCredential(auth, "access_token")
	if gitLabRuntimeCredential(auth, "auth_method") == "pat" || token == "" {
		token = gitLabRuntimeCredential(auth, "personal_access_token")
	}
	if token == "" {
		return ordinaryProviderStop(http.StatusUnauthorized, "GitLab OAuth access token or personal access token is required")
	}
	req.Header = http.Header{"Authorization": {"Bearer " + token}, "Content-Type": {"application/json"}, "Accept": {"application/json"}}
	return nil
}

func (e *GitLabExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, ordinaryProviderStop(http.StatusBadRequest, "GitLab request is missing")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	req = req.Clone(ctx)
	if err := e.PrepareRequest(req, auth); err != nil {
		return nil, err
	}
	return ordinaryProviderClient(ctx, e.cfg, auth).Do(req)
}

func (e *GitLabExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ cliproxyexecutor.Response, err error) {
	if opts.Stream || strings.TrimSpace(req.Model) != GitLabCodeSuggestionsModel {
		return cliproxyexecutor.Response{}, ordinaryProviderStop(http.StatusNotImplemented, "GitLab supports the explicit code suggestions model and synchronous current_file requests")
	}
	var body struct {
		CurrentFile     json.RawMessage `json:"current_file"`
		Intent          json.RawMessage `json:"intent,omitempty"`
		Context         json.RawMessage `json:"context,omitempty"`
		UserInstruction json.RawMessage `json:"user_instruction,omitempty"`
		Messages        json.RawMessage `json:"messages,omitempty"`
	}
	var file struct {
		FileName           string  `json:"file_name"`
		ContentAboveCursor *string `json:"content_above_cursor"`
	}
	if err := json.Unmarshal(req.Payload, &body); err != nil ||
		json.Unmarshal(body.CurrentFile, &file) != nil || strings.TrimSpace(file.FileName) == "" ||
		file.ContentAboveCursor == nil || len(body.Messages) != 0 {
		return cliproxyexecutor.Response{}, ordinaryProviderStop(http.StatusBadRequest, "GitLab Code Suggestions requires current_file.file_name and current_file.content_above_cursor")
	}
	base, err := gitLabRuntimeBaseURL(auth)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return cliproxyexecutor.Response{}, ordinaryProviderStop(http.StatusBadRequest, "invalid GitLab Code Suggestions request")
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	defer reporter.TrackFailure(ctx, &err)
	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v4/code_suggestions/completions", bytes.NewReader(payload))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	resp, err := e.HttpRequest(ctx, auth, upstream)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rejected := statusErr{code: resp.StatusCode, msg: "GitLab Code Suggestions rejected the request",
			retryAfter: parseRetryAfterHeader(resp.Header, time.Now())}
		return cliproxyexecutor.Response{}, ordinaryProviderRefusal{rejected}
	}
	payload, err = io.ReadAll(io.LimitReader(resp.Body, maxGitLabSuggestionResponse+1))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if len(payload) > maxGitLabSuggestionResponse || !json.Valid(payload) {
		return cliproxyexecutor.Response{}, ordinaryProviderStop(http.StatusBadGateway, "invalid GitLab Code Suggestions response")
	}
	var metrics struct {
		Metadata struct {
			Model struct {
				Usage struct {
					Input  int64 `json:"input_tokens"`
					Output int64 `json:"output_tokens"`
				} `json:"tokens_consumption_metadata"`
			} `json:"model"`
		} `json:"metadata"`
	}
	if json.Unmarshal(payload, &metrics) == nil {
		count := metrics.Metadata.Model.Usage
		reporter.Publish(ctx, usage.Detail{InputTokens: count.Input, OutputTokens: count.Output, TotalTokens: count.Input + count.Output})
	}
	reporter.EnsurePublished(ctx)
	return cliproxyexecutor.Response{Payload: payload, Headers: resp.Header.Clone()}, nil
}

func (e *GitLabExecutor) ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, ordinaryProviderStop(http.StatusNotImplemented, "GitLab Code Suggestions completions do not stream")
}

func (e *GitLabExecutor) CountTokens(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, ordinaryProviderStop(http.StatusNotImplemented, "GitLab Code Suggestions does not expose token counting")
}

func (e *GitLabExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	refresh := gitLabRuntimeCredential(auth, "refresh_token")
	if refresh == "" || gitLabRuntimeCredential(auth, "auth_method") == "pat" {
		return auth, nil
	}
	base, err := gitLabRuntimeBaseURL(auth)
	if err != nil {
		return auth, err
	}
	clientID := gitLabRuntimeCredential(auth, "oauth_client_id")
	if clientID == "" {
		return auth, ordinaryProviderStop(http.StatusUnauthorized, "GitLab OAuth application ID is missing")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client := gitlab.NewAuthClientWithHTTPClient(ordinaryProviderClient(ctx, e.cfg, auth))
	token, err := client.RefreshTokens(ctx, base, clientID, gitLabRuntimeCredential(auth, "oauth_client_secret"), refresh)
	if err != nil || token == nil || strings.TrimSpace(token.AccessToken) == "" {
		if ctx.Err() != nil {
			return auth, ctx.Err()
		}
		return auth, ordinaryProviderStop(http.StatusBadGateway, "GitLab OAuth refresh failed")
	}
	next := auth.Clone()
	next.Metadata["access_token"] = token.AccessToken
	if token.RefreshToken != "" {
		next.Metadata["refresh_token"] = token.RefreshToken
	}
	now := time.Now().UTC()
	next.Metadata["last_refresh"] = now.Format(time.RFC3339)
	if expiry := gitlab.TokenExpiry(now, token); !expiry.IsZero() {
		next.Metadata["oauth_expires_at"] = expiry.Format(time.RFC3339)
	}
	return next, nil
}

func gitLabRuntimeBaseURL(auth *cliproxyauth.Auth) (string, error) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "gitlab") ||
		auth.Disabled || auth.Status == cliproxyauth.StatusDisabled {
		return "", ordinaryProviderStop(http.StatusUnauthorized, "an enabled GitLab credential is required")
	}
	configured := gitlab.NormalizeBaseURL(os.Getenv("GITLAB_BASE_URL"))
	target, err := url.Parse(configured)
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil ||
		target.RawQuery != "" || target.ForceQuery || target.Fragment != "" {
		return "", ordinaryProviderStop(http.StatusServiceUnavailable, "the configured GitLab HTTPS endpoint is invalid")
	}
	if saved := gitLabRuntimeCredential(auth, "base_url"); saved == "" || gitlab.NormalizeBaseURL(saved) != configured {
		return "", ordinaryProviderStop(http.StatusForbidden, "the GitLab credential belongs to a different configured instance")
	}
	return configured, nil
}

func gitLabRuntimeCredential(auth *cliproxyauth.Auth, key string) string {
	if auth == nil {
		return ""
	}
	value, _ := auth.Metadata[key].(string)
	return strings.TrimSpace(value)
}

func ordinaryProviderClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) *http.Client {
	client := *helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

type ordinaryProviderRefusal struct{ statusErr }

func (ordinaryProviderRefusal) IsRequestStop() bool { return true }

func ordinaryProviderStop(status int, message string) ordinaryProviderRefusal {
	return ordinaryProviderRefusal{statusErr{code: status, msg: message}}
}

func preserveOrdinaryProviderRefusal(err error) error {
	var status statusErr
	if errors.As(err, &status) && status.code >= 300 && status.code <= 599 {
		return ordinaryProviderRefusal{status}
	}
	return err
}
