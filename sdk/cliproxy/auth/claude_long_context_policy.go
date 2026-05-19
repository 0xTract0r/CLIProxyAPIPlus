package auth

import (
	"fmt"
	"net/http"
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

const (
	claudeSonnetNormalContextTokens = int64(200_000)
	claudeMaxContextTokens          = int64(1_000_000)
)

func (m *Manager) guardClaudeLongContextPolicy(providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
	if !containsProvider(providers, "claude") {
		return nil
	}
	inputTokens, ok := estimateClaudePolicyInputTokens(req.Payload)
	if !ok || inputTokens <= claudeSonnetNormalContextTokens {
		return nil
	}

	requestedModel := requestedModelForPolicy(req, opts)
	if inputTokens > claudeMaxContextTokens {
		return &Error{
			Code:       "request_too_large",
			Message:    fmt.Sprintf("Claude request is estimated at %d input tokens, above the 1M context limit. Compact or clear context before retrying. No routing was attempted; requested model %q was not changed.", inputTokens, requestedModel),
			HTTPStatus: http.StatusRequestEntityTooLarge,
		}
	}

	if !isClaudeSonnetPolicyModel(requestedModel) && !isClaudeSonnetPolicyModel(req.Model) {
		return nil
	}

	policy := m.claudeSonnetLongContextPolicy()
	return newClaudeSonnetLongContextPolicyError(policy, requestedModel, inputTokens)
}

func (m *Manager) claudeSonnetLongContextPolicy() string {
	if m == nil {
		return internalconfig.ClaudeSonnetLongContextPolicyFailWithHint
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		return internalconfig.ClaudeSonnetLongContextPolicyFailWithHint
	}
	return internalconfig.NormalizeClaudeSonnetLongContextPolicy(cfg.Claude.SonnetLongContextPolicy)
}

func newClaudeSonnetLongContextPolicyError(policy, requestedModel string, inputTokens int64) error {
	message := ""
	switch policy {
	case internalconfig.ClaudeSonnetLongContextPolicyCompact:
		message = fmt.Sprintf("Claude Sonnet request is estimated at %d input tokens, above the normal 200K context window. claude.sonnet_long_context_policy=compact_required requires the client to compact or clear context before retrying. Requested model %q was not changed.", inputTokens, requestedModel)
	case internalconfig.ClaudeSonnetLongContextPolicyRouteToOpus1M:
		message = fmt.Sprintf("Claude Sonnet request is estimated at %d input tokens, above the normal 200K context window. claude.sonnet_long_context_policy=route_to_opus_1m is recognized, but automatic Opus routing is not implemented in this build, so the request was rejected instead of silently changing models. Use opus[1m], compact context, or enable Claude extra usage. Requested model %q was not changed.", inputTokens, requestedModel)
	default:
		message = fmt.Sprintf("Claude Sonnet request is estimated at %d input tokens, above the normal 200K context window. Sonnet 1M requires Claude extra usage. Use opus[1m], compact or clear context, or explicitly enable Claude extra usage before retrying. Requested model %q was not changed.", inputTokens, requestedModel)
	}
	return &Error{
		Code:       "invalid_request_error",
		Message:    message,
		HTTPStatus: http.StatusBadRequest,
	}
}

func requestedModelForPolicy(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	if raw := requestedModelMetadataValue(opts.Metadata); raw != "" {
		return raw
	}
	if model := strings.TrimSpace(req.Model); model != "" {
		return model
	}
	model := strings.TrimSpace(gjson.GetBytes(req.Payload, "model").String())
	if model != "" {
		return model
	}
	return "unknown"
}

func requestedModelMetadataValue(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw := meta[cliproxyexecutor.RequestedModelMetadataKey]
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func isClaudeSonnetPolicyModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	return strings.Contains(model, "sonnet") && (strings.Contains(model, "claude") || strings.HasPrefix(model, "sonnet"))
}

func estimateClaudePolicyInputTokens(payload []byte) (int64, bool) {
	if len(payload) == 0 {
		return 0, false
	}
	root := gjson.ParseBytes(payload)
	var total int64
	total += estimateClaudePolicyValueTokens(root.Get("system"))
	total += estimateClaudePolicyMessagesTokens(root.Get("messages"))
	total += estimateClaudePolicyValueTokens(root.Get("tools"))
	if total <= 0 {
		total = estimateBytesAsTokens(len(payload))
	}
	return total, total > 0
}

func estimateClaudePolicyMessagesTokens(messages gjson.Result) int64 {
	if !messages.Exists() || !messages.IsArray() {
		return 0
	}
	var total int64
	messages.ForEach(func(_, message gjson.Result) bool {
		total += estimateClaudePolicyValueTokens(message.Get("content"))
		return true
	})
	return total
}

func estimateClaudePolicyValueTokens(value gjson.Result) int64 {
	if !value.Exists() {
		return 0
	}
	switch {
	case value.Type == gjson.String:
		return estimateBytesAsTokens(len(value.String()))
	case value.IsArray():
		var total int64
		value.ForEach(func(_, item gjson.Result) bool {
			total += estimateClaudePolicyValueTokens(item)
			return true
		})
		return total
	case value.Type == gjson.JSON:
		if text := value.Get("text"); text.Exists() {
			return estimateClaudePolicyValueTokens(text)
		}
		if content := value.Get("content"); content.Exists() {
			return estimateClaudePolicyValueTokens(content)
		}
		if value.Get("type").String() == "image" {
			return 1000
		}
		return estimateBytesAsTokens(len(value.Raw))
	default:
		return estimateBytesAsTokens(len(value.Raw))
	}
}

func estimateBytesAsTokens(n int) int64 {
	if n <= 0 {
		return 0
	}
	return int64((n + 3) / 4)
}
