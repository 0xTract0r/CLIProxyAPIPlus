package executor

import (
	"context"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type upstreamRequestLog = helps.UpstreamRequestLog
type sensitiveWordMatcher = helps.SensitiveWordMatcher
type tokenizerWrapper = helps.TokenizerWrapper

func newProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	return helps.NewProxyAwareHTTPClient(ctx, cfg, auth, timeout)
}

func withRuntimeTransportHostFromRequest(ctx context.Context, req *http.Request) context.Context {
	return helps.WithRuntimeTransportHostFromRequest(ctx, req)
}

func payloadRequestedModel(opts cliproxyexecutor.Options, fallback string) string {
	return helps.PayloadRequestedModel(opts, fallback)
}

func applyPayloadConfigWithRoot(cfg *config.Config, model, protocol, root string, payload, original []byte, requestedModel string) []byte {
	return helps.ApplyPayloadConfigWithRoot(cfg, model, protocol, root, payload, original, requestedModel)
}

func recordAPIRequest(ctx context.Context, cfg *config.Config, info upstreamRequestLog) {
	helps.RecordAPIRequest(ctx, cfg, info)
}

func recordAPIResponseMetadata(ctx context.Context, cfg *config.Config, status int, headers http.Header) {
	helps.RecordAPIResponseMetadata(ctx, cfg, status, headers)
}

func recordAPIResponseError(ctx context.Context, cfg *config.Config, err error) {
	helps.RecordAPIResponseError(ctx, cfg, err)
}

func appendAPIResponseChunk(ctx context.Context, cfg *config.Config, chunk []byte) {
	helps.AppendAPIResponseChunk(ctx, cfg, chunk)
}

func summarizeErrorBody(contentType string, body []byte) string {
	return helps.SummarizeErrorBody(contentType, body)
}

func logWithRequestID(ctx context.Context) *log.Entry {
	return helps.LogWithRequestID(ctx)
}

func jsonPayload(line []byte) []byte {
	return helps.JSONPayload(line)
}

func isClaudeCodeClient(userAgent string) bool {
	return helps.IsClaudeCodeClient(userAgent)
}

func cachedUserID(apiKey string) string {
	return helps.CachedUserID(apiKey)
}

func generateFakeUserID() string {
	return helps.GenerateFakeUserID()
}

func isValidUserID(userID string) bool {
	return helps.IsValidUserID(userID)
}

func shouldCloak(cloakMode string, userAgent string) bool {
	return helps.ShouldCloak(cloakMode, userAgent)
}

func buildSensitiveWordMatcher(words []string) *sensitiveWordMatcher {
	return helps.BuildSensitiveWordMatcher(words)
}

func obfuscateSensitiveWords(payload []byte, matcher *sensitiveWordMatcher) []byte {
	return helps.ObfuscateSensitiveWords(payload, matcher)
}

func getTokenizer(model string) (*tokenizerWrapper, error) {
	return helps.GetTokenizer(model)
}

func tokenizerForModel(model string) (*tokenizerWrapper, error) {
	return helps.TokenizerForModel(model)
}

func countOpenAIChatTokens(enc *tokenizerWrapper, payload []byte) (int64, error) {
	return helps.CountOpenAIChatTokens(enc, payload)
}

func countClaudeChatTokens(enc *tokenizerWrapper, payload []byte) (int64, error) {
	return helps.CountClaudeChatTokens(enc, payload)
}

func buildOpenAIUsageJSON(count int64) []byte {
	return helps.BuildOpenAIUsageJSON(count)
}

func collectOpenAIContent(content gjson.Result, segments *[]string) {
	helps.CollectOpenAIContent(content, segments)
}
