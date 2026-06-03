package executor

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// usageReporter preserves the pre-refactor executor-local helper API while
// delegating to the new helps package exported surface.
type usageReporter struct {
	inner *helps.UsageReporter
}

func newUsageReporter(ctx context.Context, provider, model string, auth *cliproxyauth.Auth) *usageReporter {
	return &usageReporter{inner: helps.NewUsageReporter(ctx, provider, model, auth)}
}

func (r *usageReporter) publish(ctx context.Context, detail usage.Detail) {
	if r == nil || r.inner == nil {
		return
	}
	r.inner.Publish(ctx, detail)
}

func (r *usageReporter) publishFailure(ctx context.Context) {
	if r == nil || r.inner == nil {
		return
	}
	r.inner.PublishFailure(ctx)
}

func (r *usageReporter) trackFailure(ctx context.Context, errPtr *error) {
	if r == nil || r.inner == nil {
		return
	}
	r.inner.TrackFailure(ctx, errPtr)
}

func (r *usageReporter) ensurePublished(ctx context.Context) {
	if r == nil || r.inner == nil {
		return
	}
	r.inner.EnsurePublished(ctx)
}

func parseOpenAIUsage(data []byte) usage.Detail {
	return helps.ParseOpenAIUsage(data)
}

func parseOpenAIStreamUsage(line []byte) (usage.Detail, bool) {
	return helps.ParseOpenAIStreamUsage(line)
}

func parseOpenAIResponsesUsage(data []byte) usage.Detail {
	return helps.ParseOpenAIResponsesUsage(data)
}

func parseOpenAIResponsesStreamUsage(line []byte) (usage.Detail, bool) {
	return helps.ParseOpenAIResponsesStreamUsage(line)
}

func parseClaudeUsage(data []byte) usage.Detail {
	return helps.ParseClaudeUsage(data)
}

func parseClaudeStreamUsage(line []byte) (usage.Detail, bool) {
	return helps.ParseClaudeStreamUsage(line)
}
