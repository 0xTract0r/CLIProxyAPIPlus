package helps

import (
	"context"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// UsageReporter exports the usage reporting helper API for executor packages
// that were migrated from the old in-package helpers to the dedicated helps package.
type UsageReporter struct {
	provider    string
	model       string
	authID      string
	authIndex   string
	apiKey      string
	source      string
	requestedAt time.Time
	once        sync.Once
}

func NewUsageReporter(ctx context.Context, provider, model string, auth *cliproxyauth.Auth) *UsageReporter {
	apiKey := APIKeyFromContext(ctx)
	reporter := &UsageReporter{
		provider:    provider,
		model:       model,
		requestedAt: time.Now(),
		apiKey:      apiKey,
		source:      resolveUsageSource(auth, apiKey),
	}
	if auth != nil {
		reporter.authID = auth.ID
		reporter.authIndex = auth.EnsureIndex()
	}
	return reporter
}

func (r *UsageReporter) Publish(ctx context.Context, detail usage.Detail) {
	if r == nil {
		return
	}
	r.publishWithOutcome(ctx, detail, false)
}

func (r *UsageReporter) PublishFailure(ctx context.Context) {
	if r == nil {
		return
	}
	r.publishWithOutcome(ctx, usage.Detail{}, true)
}

func (r *UsageReporter) TrackFailure(ctx context.Context, errPtr *error) {
	if r == nil || errPtr == nil {
		return
	}
	if *errPtr != nil {
		r.PublishFailure(ctx)
	}
}

func (r *UsageReporter) EnsurePublished(ctx context.Context) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		usage.PublishRecord(ctx, r.buildRecord(usage.Detail{}, false))
	})
}

func (r *UsageReporter) publishWithOutcome(ctx context.Context, detail usage.Detail, failed bool) {
	if r == nil {
		return
	}
	if detail.TotalTokens == 0 {
		total := detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
		if total > 0 {
			detail.TotalTokens = total
		}
	}
	r.once.Do(func() {
		usage.PublishRecord(ctx, r.buildRecord(detail, failed))
	})
}

func (r *UsageReporter) buildRecord(detail usage.Detail, failed bool) usage.Record {
	if r == nil {
		return usage.Record{Detail: detail, Failed: failed}
	}
	return usage.Record{
		Provider:    r.provider,
		Model:       r.model,
		Source:      r.source,
		APIKey:      r.apiKey,
		AuthID:      r.authID,
		AuthIndex:   r.authIndex,
		RequestedAt: r.requestedAt,
		Latency:     r.latency(),
		Failed:      failed,
		Detail:      detail,
	}
}

func (r *UsageReporter) latency() time.Duration {
	if r == nil || r.requestedAt.IsZero() {
		return 0
	}
	latency := time.Since(r.requestedAt)
	if latency < 0 {
		return 0
	}
	return latency
}

func APIKeyFromContext(ctx context.Context) string {
	return apiKeyFromContext(ctx)
}

func ParseCodexUsage(data []byte) (usage.Detail, bool) {
	return parseCodexUsage(data)
}

func ParseOpenAIUsage(data []byte) usage.Detail {
	return parseOpenAIUsage(data)
}

func ParseOpenAIStreamUsage(line []byte) (usage.Detail, bool) {
	return parseOpenAIStreamUsage(line)
}

func ParseOpenAIResponsesUsage(data []byte) usage.Detail {
	return parseOpenAIResponsesUsage(data)
}

func ParseOpenAIResponsesStreamUsage(line []byte) (usage.Detail, bool) {
	return parseOpenAIResponsesStreamUsage(line)
}

func ParseClaudeUsage(data []byte) usage.Detail {
	return parseClaudeUsage(data)
}

func ParseClaudeStreamUsage(line []byte) (usage.Detail, bool) {
	return parseClaudeStreamUsage(line)
}

func ParseGeminiCLIUsage(data []byte) usage.Detail {
	return parseGeminiCLIUsage(data)
}

func ParseGeminiUsage(data []byte) usage.Detail {
	return parseGeminiUsage(data)
}

func ParseGeminiStreamUsage(line []byte) (usage.Detail, bool) {
	return parseGeminiStreamUsage(line)
}

func ParseGeminiCLIStreamUsage(line []byte) (usage.Detail, bool) {
	return parseGeminiCLIStreamUsage(line)
}

func ParseAntigravityUsage(data []byte) usage.Detail {
	return parseAntigravityUsage(data)
}

func ParseAntigravityStreamUsage(line []byte) (usage.Detail, bool) {
	return parseAntigravityStreamUsage(line)
}

func JSONPayload(line []byte) []byte {
	return jsonPayload(line)
}
