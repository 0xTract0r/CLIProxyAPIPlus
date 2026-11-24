package usage

import (
	"errors"
	"math"
)

var (
	errInvalidUsageTokens = errors.New("usage details contain negative or overflowing token counts")
	errInvalidUsageDetail = errors.New("usage details contain an invalid timestamp or cost")
	errUsageCapacity      = errors.New("usage totals would exceed the supported range")
)

func checkedUsageTokens(tokens TokenStats) (TokenStats, error) {
	for _, count := range [...]int64{
		tokens.InputTokens, tokens.OutputTokens, tokens.ReasoningTokens, tokens.CachedTokens,
		tokens.CacheReadTokens, tokens.CacheWriteTokens, tokens.TotalTokens, tokens.BillableTokens,
	} {
		if count < 0 {
			return TokenStats{}, errInvalidUsageTokens
		}
	}
	if tokens.TotalTokens == 0 {
		if tokens.OutputTokens > math.MaxInt64-tokens.InputTokens ||
			tokens.ReasoningTokens > math.MaxInt64-tokens.InputTokens-tokens.OutputTokens {
			return TokenStats{}, errInvalidUsageTokens
		}
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	}
	cacheRead := maxInt64(tokens.CachedTokens, tokens.CacheReadTokens)
	if cacheRead > math.MaxInt64-tokens.TotalTokens || tokens.CacheWriteTokens > math.MaxInt64-tokens.TotalTokens-cacheRead {
		return TokenStats{}, errInvalidUsageTokens
	}
	return normaliseTokenStats(tokens), nil
}

func checkUsageDetail(detail RequestDetail) error {
	if _, err := detail.Timestamp.MarshalJSON(); err != nil {
		return errInvalidUsageDetail
	}
	if detail.CostUSD < 0 || math.IsNaN(detail.CostUSD) || math.IsInf(detail.CostUSD, 0) {
		return errInvalidUsageDetail
	}
	return nil
}

type usageCapacity struct {
	requests, tokens, billable, costMicros int64
}

// All counters updated by ingestion are nonnegative subsets of these totals.
// Callers hold mu from the capacity check through publication.
func (s *RequestStatistics) usageCapacityLocked() usageCapacity {
	return usageCapacity{
		requests:   math.MaxInt64 - s.totalRequests,
		tokens:     math.MaxInt64 - s.totalTokens,
		billable:   math.MaxInt64 - s.totalBillableTokens,
		costMicros: math.MaxInt64 - s.totalCostMicros,
	}
}

func (remaining *usageCapacity) consume(tokens TokenStats, pricing pricingTotals) bool {
	if remaining.requests <= 0 || tokens.TotalTokens > remaining.tokens ||
		tokens.BillableTokens > remaining.billable || pricing.CostMicros < 0 ||
		pricing.CostMicros > remaining.costMicros {
		return false
	}
	remaining.requests--
	remaining.tokens -= tokens.TotalTokens
	remaining.billable -= tokens.BillableTokens
	remaining.costMicros -= pricing.CostMicros
	return true
}
