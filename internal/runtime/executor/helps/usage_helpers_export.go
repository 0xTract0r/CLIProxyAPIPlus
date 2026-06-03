package helps

import (
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// ParseOpenAIResponsesUsage and ParseOpenAIResponsesStreamUsage remain fork-specific
// exported wrappers after the upstream usage-helper migration. Every other re-export
// that used to live here (UsageReporter, NewUsageReporter, APIKeyFromContext, the
// Parse*Usage family, JSONPayload, ...) is now defined directly in usage_helpers.go,
// so keeping them here would redeclare those symbols.

// ParseOpenAIResponsesUsage parses a full OpenAI Responses API usage payload.
func ParseOpenAIResponsesUsage(data []byte) usage.Detail {
	return parseOpenAIResponsesUsage(data)
}

// ParseOpenAIResponsesStreamUsage parses a single OpenAI Responses API stream line.
func ParseOpenAIResponsesStreamUsage(line []byte) (usage.Detail, bool) {
	return parseOpenAIResponsesStreamUsage(line)
}
