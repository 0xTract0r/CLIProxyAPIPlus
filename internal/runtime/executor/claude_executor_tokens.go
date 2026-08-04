package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (e *ClaudeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx = contextWithClaudeInboundHeaders(ctx, opts.Headers)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	upstreamModel := e.upstreamModel(baseModel)

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")
	// Use streaming translation to preserve function calling, except for claude.
	stream := from != to
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, stream)
	body, _ = sjson.SetBytes(body, "model", upstreamModel)
	if rebuildMidSystemMessageEnabled(e.cfg, auth) {
		body = rebuildMidSystemMessagesToTopLevel(body)
	}

	// 反关联修复 A（C1）：count_tokens 必须与 messages 走同一套 cch 签名路径。
	// 真实 claude-cli 的 count_tokens 与 messages 共用同一个 SDK client，billing-header
	// 用"cch=00000 占位再 xxHash64 回填"模式，两端点同一套 cch。此前 count_tokens 硬钉
	// 非签名模式（generateBillingHeader 走 sha256[:5]）且从不调用 signAnthropicMessagesBody，
	// 导致同一 OAuth 账号 messages=xxHash64 / count_tokens=sha256，成为跨端点分辨信号。
	// 这里对 OAuth / experimentalCCHSigning 启用与 messages 完全一致的条件与参数：
	//   - 签名模式占位 cch=00000（与 messages 的 applyCloaking 路径一致）
	//   - 使用与 messages 相同的 entrypoint（从 UA 解析）/ workload（从 ctx 取）
	// 真正的 xxHash64 回填在 body 全部规范化（device_id ⑦ env）之后进行（见下方）。
	oauthToken := isClaudeOAuthToken(apiKey)
	useCCHSigning := oauthToken || experimentalCCHSigningEnabled(e.cfg, auth)
	if !strings.HasPrefix(baseModel, "claude-3-5-haiku") {
		billingVersion := resolveClaudeBillingVersion(ctx, e.cfg, auth, apiKey)
		if useCCHSigning {
			clientUserAgent := getClientUserAgent(ctx)
			entrypoint := parseEntrypointFromUA(e.cfg, clientUserAgent)
			workload := getWorkloadFromContext(ctx)
			body = checkSystemInstructionsWithSigningMode(body, false, useCCHSigning, oauthToken, billingVersion, entrypoint, workload)
		} else {
			// 非签名（纯 API key）路径保持原状：messages 本来也是非签名 sha256，
			// count_tokens 维持 sha256 即与 messages 一致，行为零变化。
			body = checkSystemInstructionsWithVersion(body, false, billingVersion)
		}
	}

	// Keep count_tokens requests compatible with Anthropic cache-control constraints too.
	body = enforceCacheControlLimit(body, 4)
	body = normalizeCacheControlTTL(body)

	// Extract betas from body and convert to header (for count_tokens too)
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	if oauthToken {
		body, _ = prepareClaudeOAuthToolNamesForUpstream(body, claudeToolPrefix, auth.ToolPrefixDisabled())
	}
	body = sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx, body, baseModel)

	// Account-scoped device_id normalization for the count_tokens path. Unlike the
	// main messages path (applyCloaking), this only rewrites an existing
	// metadata.user_id.device_id and never fabricates the field when it is absent:
	// the real claude-cli count_tokens fingerprint is not yet captured, so emitting
	// an extra metadata.user_id we are not sure the client sends could itself become
	// a detection signal. Existing user_id objects still get their device_id swapped
	// to the same account-derived value used by Execute; a parse failure is a safe
	// pass-through (never a 400).
	countTokensAuthDir := ""
	if e.cfg != nil {
		countTokensAuthDir = e.cfg.AuthDir
	}
	body = helps.InjectAccountDeviceIDWithOptions(body, countTokensAuthDir, auth, apiKey, false)

	// 反关联修复 A（C1）续：在 body 完成全部规范化（sanitize / device_id ⑦ / env）之后，
	// 与 messages 路径（Execute / ExecuteStream）完全相同地回填 cch。
	// signAnthropicMessagesBody 会先把 billing-header 的 cch 归一为 00000 再做 xxHash64，
	// 因此只要上面用签名模式注入了 cch=00000 占位且其余 billing 字段（cc_version /
	// build / entrypoint / workload）与 messages 一致，同一逻辑请求两端点得到同一 cch。
	if useCCHSigning {
		body = signAnthropicMessagesBody(body)
	}

	url := fmt.Sprintf("%s/v1/messages/count_tokens?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, e.cfg)
	// claude 版本高水位持久化（真实 serving count_tokens 路径）：count_tokens 也经 applyClaudeHeaders
	// 记入内存观测，接上写回更全且单调抬升幂等无害（稳态零写盘）。
	e.persistClaudeDeviceHighWater(auth, apiKey)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.upstreamRequestLogProvider(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	ctx = helps.WithRuntimeTransportHostFromRequest(ctx, httpReq)
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := doClaudeHTTPWithTransportRetry(ctx, httpClient, httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, claudeUpstreamTransportError(err)
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Decompress error responses — pass the Content-Encoding value (may be empty)
		// and let decodeResponseBody handle both header-declared and magic-byte-detected
		// compression.  This keeps error-path behaviour consistent with the success path.
		errBody, decErr := decodeResponseBody(resp.Body, resp.Header.Get("Content-Encoding"))
		if decErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, decErr)
			msg := fmt.Sprintf("failed to decode error response body: %v", decErr)
			logWithRequestID(ctx).Warn(msg)
			return cliproxyexecutor.Response{}, newClaudeStatusErr(resp.StatusCode, []byte(msg), resp.Header, time.Now())
		}
		b, readErr := io.ReadAll(errBody)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, newClaudeStatusErr(resp.StatusCode, b, resp.Header, time.Now())
	}
	decodedBody, err := decodeResponseBody(resp.Body, resp.Header.Get("Content-Encoding"))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	count := gjson.GetBytes(data, "input_tokens").Int()
	out := sdktranslator.TranslateTokenCount(ctx, to, from, count, data)
	return cliproxyexecutor.Response{Payload: out, Headers: resp.Header.Clone()}, nil
}
