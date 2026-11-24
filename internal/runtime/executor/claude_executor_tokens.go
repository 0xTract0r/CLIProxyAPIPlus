package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func (e *ClaudeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := ctx.Err(); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	body, err := e.prepareOrdinaryClaudeMessages(ctx, auth, req, opts, false, false, baseModel, e.upstreamModel(baseModel))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if err := validateClaudeTokenCountRequest(body); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if len(body) > ordinaryClaudeMaxRequestBytes {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusRequestEntityTooLarge, msg: "Claude request exceeds size limit", requestScoped: true}
	}

	key, base, _ := ordinaryClaudeCredential(auth)
	if shouldUseClaudeUpstreamTokenCount(key, base) {
		return e.sendOrdinaryClaudeTokenCount(ctx, auth, body, opts)
	}
	count, err := helps.CountClaudeInputTokens(body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("claude token counting failed: %w", err)
	}
	return ordinaryClaudeTokenCountResponse(ctx, opts, count, []byte(fmt.Sprintf(`{"input_tokens":%d}`, count)), nil), nil
}

// countTokensUpstream is also used by a Messages-compatible delegated
// implementation that explicitly supports counting on its configured origin.
func (e *ClaudeExecutor) countTokensUpstream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	body, err := e.prepareOrdinaryClaudeMessages(ctx, auth, req, opts, false, false, baseModel, e.upstreamModel(baseModel))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if err = validateClaudeTokenCountRequest(body); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return e.sendOrdinaryClaudeTokenCount(ctx, auth, body, opts)
}

func (e *ClaudeExecutor) sendOrdinaryClaudeTokenCount(ctx context.Context, auth *cliproxyauth.Auth, body []byte, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	doc := ojson.NewDoc(body)
	for _, field := range []string{"stream", "max_tokens", "temperature", "top_k", "top_p", "stop_sequences"} {
		_ = doc.Delete(field)
	}
	key, base, _ := ordinaryClaudeCredential(auth)
	if shouldUseClaudeUpstreamTokenCount(key, base) {
		for _, field := range []string{"metadata", "context_management", "diagnostics"} {
			_ = doc.Delete(field)
		}
	}
	resp, err := e.sendOrdinaryClaudeRequest(ctx, auth, "/v1/messages/count_tokens", doc.Bytes(), false, opts.Headers, nil)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return cliproxyexecutor.Response{}, ordinaryClaudeHTTPError(resp)
	}
	usageJSON, err := readOrdinaryClaudeJSON(resp, 64<<10)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	result := ojson.GetBytesNoCopy(usageJSON, "input_tokens")
	if result.Type != ojson.Number || result.Int() < 0 || result.Num != float64(result.Int()) {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadGateway, msg: "invalid Claude token count response"}
	}
	return ordinaryClaudeTokenCountResponse(ctx, opts, result.Int(), usageJSON, resp.Header.Clone()), nil
}

func ordinaryClaudeTokenCountResponse(ctx context.Context, opts cliproxyexecutor.Options, count int64, usageJSON []byte, headers http.Header) cliproxyexecutor.Response {
	translated := sdktranslator.TranslateTokenCount(ctx, sdktranslator.FromString("claude"), cliproxyexecutor.ResponseFormatOrSource(opts), count, usageJSON)
	return cliproxyexecutor.Response{Payload: translated, Headers: headers}
}

func shouldUseClaudeUpstreamTokenCount(key, base string) bool {
	if strings.TrimSpace(key) == "" {
		return false
	}
	u, err := parseOrdinaryClaudeBase(base)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "api.anthropic.com") &&
		(u.Port() == "" || u.Port() == "443") && strings.TrimRight(u.Path, "/") == ""
}

type claudeTokenCountValidationError struct {
	statusErr
}

func (claudeTokenCountValidationError) IsRequestScoped() bool {
	return true
}

func newClaudeTokenCountValidationError(message string) error {
	return claudeTokenCountValidationError{statusErr{code: http.StatusBadRequest, msg: message}}
}

func validateClaudeTokenCountRequest(body []byte) error {
	if !ojson.ValidBytes(body) {
		return newClaudeTokenCountValidationError("invalid Claude token count request JSON")
	}
	root := util.ParseGJSONBytesNoCopy(body)
	if !root.IsObject() {
		return newClaudeTokenCountValidationError("Claude token count request must be a JSON object")
	}
	system := root.Get("system")
	if system.Exists() && system.Type != ojson.String {
		if !system.IsArray() {
			return newClaudeTokenCountValidationError("Claude token count request system must be a string or text blocks")
		}
		for _, block := range system.Array() {
			if !block.IsObject() || block.Get("type").String() != "text" || block.Get("text").Type != ojson.String {
				return newClaudeTokenCountValidationError("Claude token count request system blocks must be text")
			}
		}
	}
	messages := root.Get("messages")
	if !messages.IsArray() || len(messages.Array()) == 0 {
		return newClaudeTokenCountValidationError("Claude token count request messages must be a non-empty array")
	}
	for _, message := range messages.Array() {
		if !message.IsObject() {
			return newClaudeTokenCountValidationError("Claude token count request messages must contain objects")
		}
		role := message.Get("role").String()
		if role != "user" && role != "assistant" {
			return newClaudeTokenCountValidationError("Claude token count request message role must be user or assistant")
		}
		content := message.Get("content")
		if content.Type == ojson.String {
			continue
		}
		if !content.IsArray() {
			return newClaudeTokenCountValidationError("Claude token count request message content must be a string or array")
		}
		for _, block := range content.Array() {
			if !block.IsObject() || block.Get("type").Type != ojson.String || block.Get("type").String() == "" {
				return newClaudeTokenCountValidationError("Claude token count request content blocks must be typed objects")
			}
		}
	}
	return nil
}
