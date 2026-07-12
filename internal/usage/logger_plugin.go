// Package usage provides usage tracking and logging functionality for the CLI Proxy API server.
// It includes plugins for monitoring API usage, token consumption, and other metrics
// to help with observability and billing purposes.
package usage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

var statisticsEnabled atomic.Bool

func init() {
	statisticsEnabled.Store(true)
	coreusage.RegisterPlugin(NewLoggerPlugin())
}

// LoggerPlugin collects in-memory request statistics for usage analysis.
// It implements coreusage.Plugin to receive usage records emitted by the runtime.
type LoggerPlugin struct {
	stats *RequestStatistics
}

// NewLoggerPlugin constructs a new logger plugin instance.
//
// Returns:
//   - *LoggerPlugin: A new logger plugin instance wired to the shared statistics store.
func NewLoggerPlugin() *LoggerPlugin { return &LoggerPlugin{stats: defaultRequestStatistics} }

// HandleUsage implements coreusage.Plugin.
// It updates the in-memory statistics store whenever a usage record is received.
//
// Parameters:
//   - ctx: The context for the usage record
//   - record: The usage record to aggregate
func (p *LoggerPlugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if !statisticsEnabled.Load() {
		return
	}
	if p == nil || p.stats == nil {
		return
	}
	p.stats.Record(ctx, record)
}

// SetStatisticsEnabled toggles whether in-memory statistics are recorded.
func SetStatisticsEnabled(enabled bool) { statisticsEnabled.Store(enabled) }

// StatisticsEnabled reports the current recording state.
func StatisticsEnabled() bool { return statisticsEnabled.Load() }

// RequestStatistics maintains aggregated request metrics in memory.
type RequestStatistics struct {
	mu sync.RWMutex

	totalRequests       int64
	successCount        int64
	failureCount        int64
	totalTokens         int64
	totalBillableTokens int64
	totalCostMicros     int64
	unpricedRequests    int64
	unfinalizedRequests int64

	persistPath string

	apis map[string]*apiStats

	requestsByDay    map[string]int64
	requestsByHour   map[int]int64
	tokensByDay      map[string]int64
	tokensByHour     map[int]int64
	costByDayMicros  map[string]int64
	costByHourMicros map[int]int64

	persistRunning atomic.Bool
	persistDirty   atomic.Bool

	catalog *PricingCatalogManager
}

// apiStats holds aggregated metrics for a single API key.
type apiStats struct {
	TotalRequests       int64
	TotalTokens         int64
	TotalBillableTokens int64
	TotalCostMicros     int64
	UnpricedRequests    int64
	UnfinalizedRequests int64
	Models              map[string]*modelStats
}

// modelStats holds aggregated metrics for a specific model within an API.
type modelStats struct {
	TotalRequests       int64
	TotalTokens         int64
	TotalBillableTokens int64
	TotalCostMicros     int64
	UnpricedRequests    int64
	UnfinalizedRequests int64
	PricingState        pricingState
	Details             []RequestDetail
}

// RequestDetail stores the timestamp, latency, and token usage for a single request.
type RequestDetail struct {
	Timestamp time.Time `json:"timestamp"`
	LatencyMs int64     `json:"latency_ms"`
	Source    string    `json:"source"`
	AuthIndex string    `json:"auth_index"`
	// RequestID is the per-request correlation id (see internal/logging.GetRequestID).
	// It may be empty when the producing context did not carry a request id
	// (e.g. synthetic/imported records); omitempty preserves compatibility.
	RequestID     string     `json:"request_id,omitempty"`
	Tokens        TokenStats `json:"tokens"`
	Failed        bool       `json:"failed"`
	CostUSD       float64    `json:"cost_usd,omitempty"`
	PricingStatus string     `json:"pricing_status,omitempty"`
}

// TokenStats captures the token usage breakdown for a request.
type TokenStats struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	CacheReadTokens  int64 `json:"cache_read_input_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_input_tokens,omitempty"`
	TotalTokens      int64 `json:"total_tokens"`
	BillableTokens   int64 `json:"billable_tokens,omitempty"`
}

// StatisticsSnapshot represents an immutable view of the aggregated metrics.
type StatisticsSnapshot struct {
	TotalRequests           int64   `json:"total_requests"`
	SuccessCount            int64   `json:"success_count"`
	FailureCount            int64   `json:"failure_count"`
	TotalTokens             int64   `json:"total_tokens"`
	TotalBillableTokens     int64   `json:"total_billable_tokens,omitempty"`
	TotalCostUSD            float64 `json:"total_cost_usd"`
	PricingStatus           string  `json:"pricing_status,omitempty"`
	UnpricedRequestCount    int64   `json:"unpriced_request_count,omitempty"`
	UnfinalizedRequestCount int64   `json:"unfinalized_request_count,omitempty"`
	UnpricedModelCount      int64   `json:"unpriced_model_count,omitempty"`
	UnfinalizedModelCount   int64   `json:"unfinalized_model_count,omitempty"`

	APIs map[string]APISnapshot `json:"apis"`

	RequestsByDay  map[string]int64   `json:"requests_by_day"`
	RequestsByHour map[string]int64   `json:"requests_by_hour"`
	TokensByDay    map[string]int64   `json:"tokens_by_day"`
	TokensByHour   map[string]int64   `json:"tokens_by_hour"`
	CostByDay      map[string]float64 `json:"cost_by_day"`
	CostByHour     map[string]float64 `json:"cost_by_hour,omitempty"`
}

// APISnapshot summarises metrics for a single API key.
type APISnapshot struct {
	TotalRequests           int64                    `json:"total_requests"`
	TotalTokens             int64                    `json:"total_tokens"`
	TotalBillableTokens     int64                    `json:"total_billable_tokens,omitempty"`
	TotalCostUSD            float64                  `json:"total_cost_usd"`
	PricingStatus           string                   `json:"pricing_status,omitempty"`
	UnpricedRequestCount    int64                    `json:"unpriced_request_count,omitempty"`
	UnfinalizedRequestCount int64                    `json:"unfinalized_request_count,omitempty"`
	Models                  map[string]ModelSnapshot `json:"models"`
}

// ModelSnapshot summarises metrics for a specific model.
type ModelSnapshot struct {
	TotalRequests           int64           `json:"total_requests"`
	TotalTokens             int64           `json:"total_tokens"`
	TotalBillableTokens     int64           `json:"total_billable_tokens,omitempty"`
	TotalCostUSD            float64         `json:"total_cost_usd"`
	PricingStatus           string          `json:"pricing_status,omitempty"`
	UnpricedRequestCount    int64           `json:"unpriced_request_count,omitempty"`
	UnfinalizedRequestCount int64           `json:"unfinalized_request_count,omitempty"`
	Details                 []RequestDetail `json:"details"`
}

type SnapshotOptions struct {
	ExcludeDetails bool
	Since          time.Time
	// DetailLimit, when > 0, caps the total number of request details
	// returned across every api/model bucket combined (a global cap, not a
	// per-bucket cap). See SnapshotPageWithOptions for the exact boundary
	// semantics used when applying this cap.
	DetailLimit int
}

// SnapshotPage wraps a StatisticsSnapshot together with pagination cursors for
// callers windowing exports by DetailLimit/Since (e.g. incremental sync clients).
type SnapshotPage struct {
	Snapshot StatisticsSnapshot
	// HasMore is true when at least one model's request-detail list was
	// truncated by DetailLimit, meaning older windowed calls with NextSince
	// as Since would surface additional details.
	HasMore bool
	// NextSince is the cursor to pass as Since on the following call to
	// continue pulling remaining details in stable, gap-free chronological
	// order. It is the earliest "last included" detail timestamp across all
	// truncated model detail lists, formatted with nanosecond precision so
	// sub-second bursts are not truncated at the boundary.
	NextSince time.Time
}

var defaultRequestStatistics = NewRequestStatistics()

// GetRequestStatistics returns the shared statistics store.
func GetRequestStatistics() *RequestStatistics { return defaultRequestStatistics }

// NewRequestStatistics constructs an empty statistics store.
func NewRequestStatistics() *RequestStatistics {
	return NewRequestStatisticsWithCatalog(defaultPricingCatalog)
}

// NewRequestStatisticsWithCatalog constructs an empty statistics store bound to a pricing catalog.
func NewRequestStatisticsWithCatalog(catalog *PricingCatalogManager) *RequestStatistics {
	if catalog == nil {
		catalog = defaultPricingCatalog
	}
	return &RequestStatistics{
		apis:             make(map[string]*apiStats),
		requestsByDay:    make(map[string]int64),
		requestsByHour:   make(map[int]int64),
		tokensByDay:      make(map[string]int64),
		tokensByHour:     make(map[int]int64),
		costByDayMicros:  make(map[string]int64),
		costByHourMicros: make(map[int]int64),
		catalog:          catalog,
	}
}

// Record ingests a new usage record and updates the aggregates.
func (s *RequestStatistics) Record(ctx context.Context, record coreusage.Record) {
	if s == nil {
		return
	}
	if !statisticsEnabled.Load() {
		return
	}
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	detail := normaliseDetail(record.Detail)
	totalTokens := detail.TotalTokens
	totalBillableTokens := detail.BillableTokens
	statsKey := record.APIKey
	if statsKey == "" {
		statsKey = resolveAPIIdentifier(ctx, record)
	}
	failed := record.Failed
	if !failed {
		failed = !resolveSuccess(ctx)
	}
	success := !failed
	modelName := record.Model
	if modelName == "" {
		modelName = "unknown"
	}
	dayKey := timestamp.Format("2006-01-02")
	hourKey := timestamp.Hour()
	pricing := s.computeDetailPricing(modelName, detail)
	requestDetail := RequestDetail{
		Timestamp: timestamp,
		LatencyMs: normaliseLatency(record.Latency),
		Source:    record.Source,
		AuthIndex: record.AuthIndex,
		RequestID: strings.TrimSpace(internallogging.GetRequestID(ctx)),
		Tokens:    detail,
		Failed:    failed,
		CostUSD:   microsToUSD(pricing.CostMicros),
	}
	if pricing.State != pricingStatePriced {
		requestDetail.PricingStatus = string(pricing.State)
	}

	s.mu.Lock()

	s.totalRequests++
	if success {
		s.successCount++
	} else {
		s.failureCount++
	}
	s.totalTokens += totalTokens
	s.totalBillableTokens += totalBillableTokens

	stats, ok := s.apis[statsKey]
	if !ok {
		stats = &apiStats{Models: make(map[string]*modelStats)}
		s.apis[statsKey] = stats
	}
	s.updateAPIStats(stats, modelName, requestDetail, pricing)

	s.requestsByDay[dayKey]++
	s.requestsByHour[hourKey]++
	s.tokensByDay[dayKey] += totalTokens
	s.tokensByHour[hourKey] += totalTokens
	s.recordPricing(pricing, dayKey, hourKey)
	s.mu.Unlock()

	s.schedulePersistence()
}

func (s *RequestStatistics) updateAPIStats(stats *apiStats, model string, detail RequestDetail, pricing pricingTotals) {
	stats.TotalRequests++
	stats.TotalTokens += detail.Tokens.TotalTokens
	stats.TotalBillableTokens += detail.Tokens.BillableTokens
	stats.TotalCostMicros += pricing.CostMicros
	stats.UnpricedRequests += pricing.Unpriced
	stats.UnfinalizedRequests += pricing.Unfinalized
	modelStatsValue, ok := stats.Models[model]
	if !ok {
		modelStatsValue = &modelStats{}
		stats.Models[model] = modelStatsValue
	}
	modelStatsValue.TotalRequests++
	modelStatsValue.TotalTokens += detail.Tokens.TotalTokens
	modelStatsValue.TotalBillableTokens += detail.Tokens.BillableTokens
	modelStatsValue.TotalCostMicros += pricing.CostMicros
	modelStatsValue.UnpricedRequests += pricing.Unpriced
	modelStatsValue.UnfinalizedRequests += pricing.Unfinalized
	modelStatsValue.PricingState = pricingStateForCounts(
		modelStatsValue.TotalRequests,
		modelStatsValue.UnpricedRequests,
		modelStatsValue.UnfinalizedRequests,
	)
	modelStatsValue.Details = append(modelStatsValue.Details, detail)
}

// Snapshot returns a copy of the aggregated metrics for external consumption.
func (s *RequestStatistics) Snapshot() StatisticsSnapshot {
	return s.SnapshotWithOptions(SnapshotOptions{})
}

// SnapshotWithOptions returns a copy of aggregated metrics and can omit or
// window per-request details for latency-sensitive management UI reads.
func (s *RequestStatistics) SnapshotWithOptions(options SnapshotOptions) StatisticsSnapshot {
	page := s.SnapshotPageWithOptions(options)
	return page.Snapshot
}

// SnapshotPageWithOptions returns a copy of aggregated metrics along with
// pagination cursors (HasMore/NextSince) describing whether the combined
// request-detail list (across every api/model bucket) was truncated by
// DetailLimit. Callers that need to pull all details across multiple
// windowed calls (e.g. incremental sync) should keep calling this with
// Since=previous NextSince until HasMore is false.
//
// DetailLimit is a *global* cap on the total number of request details
// returned across all api/model buckets combined, not a per-bucket cap.
// This matters because export payloads are consumed as one flat page: a
// per-bucket limit lets each (api, model) pair independently return up to
// DetailLimit records, so a snapshot with many buckets can return an
// unbounded multiple of DetailLimit records in a single response (observed
// in production: 124k+ details in a single limit=5000 export page).
//
// To honour the global cap, this walks every bucket's filtered/sorted
// details twice: once to determine the single global cutoff timestamp that
// keeps the page at (or, for a tied boundary, only slightly above) size
// DetailLimit, and once to build the actual per-bucket, cutoff-bounded
// result. See detailPageCutoff for the exact boundary semantics.
func (s *RequestStatistics) SnapshotPageWithOptions(options SnapshotOptions) SnapshotPage {
	result := StatisticsSnapshot{}
	page := SnapshotPage{Snapshot: result}
	if s == nil {
		return page
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	result.TotalRequests = s.totalRequests
	result.SuccessCount = s.successCount
	result.FailureCount = s.failureCount
	result.TotalTokens = s.totalTokens
	result.TotalBillableTokens = s.totalBillableTokens
	result.TotalCostUSD = microsToUSD(s.totalCostMicros)
	result.UnpricedRequestCount = s.unpricedRequests
	result.UnfinalizedRequestCount = s.unfinalizedRequests
	result.PricingStatus = pricingStatusString(
		pricingStateForCounts(result.TotalRequests, result.UnpricedRequestCount, result.UnfinalizedRequestCount),
	)

	// Pass 1: sort+filter (Since only, no limit) every bucket once and reuse
	// the ordered slices in pass 2, so buckets are only sorted a single time
	// regardless of how many passes are needed to compute the global cutoff.
	buckets := make([]usageDetailBucket, 0, len(s.apis))
	totalFiltered := 0
	for apiName, stats := range s.apis {
		for modelName, modelStatsValue := range stats.Models {
			if options.ExcludeDetails {
				buckets = append(buckets, usageDetailBucket{apiName: apiName, modelName: modelName})
				continue
			}
			ordered := filterAndSortRequestDetails(modelStatsValue.Details, options.Since)
			buckets = append(buckets, usageDetailBucket{apiName: apiName, modelName: modelName, ordered: ordered})
			totalFiltered += len(ordered)
		}
	}

	// Pass 2: compute the single global cutoff (if any) across all buckets
	// combined, then apply it uniformly so the returned page never exceeds
	// (or only ties-exceeds, see detailPageCutoff) DetailLimit in total.
	hasMore, nextSince, cutoff, cutoffSet := detailPageCutoff(buckets, totalFiltered, options.DetailLimit)

	result.APIs = make(map[string]APISnapshot, len(s.apis))
	apiSnapshots := make(map[string]APISnapshot, len(s.apis))
	for _, bucket := range buckets {
		apiSnapshot, ok := apiSnapshots[bucket.apiName]
		if !ok {
			stats := s.apis[bucket.apiName]
			apiSnapshot = APISnapshot{
				TotalRequests:           stats.TotalRequests,
				TotalTokens:             stats.TotalTokens,
				TotalBillableTokens:     stats.TotalBillableTokens,
				TotalCostUSD:            microsToUSD(stats.TotalCostMicros),
				PricingStatus:           pricingStatusString(pricingStateForCounts(stats.TotalRequests, stats.UnpricedRequests, stats.UnfinalizedRequests)),
				UnpricedRequestCount:    stats.UnpricedRequests,
				UnfinalizedRequestCount: stats.UnfinalizedRequests,
				Models:                  make(map[string]ModelSnapshot, len(stats.Models)),
			}
		}

		modelStatsValue := s.apis[bucket.apiName].Models[bucket.modelName]
		var requestDetails []RequestDetail
		if options.ExcludeDetails {
			requestDetails = []RequestDetail{}
		} else if cutoffSet {
			requestDetails = applyDetailPageCutoff(bucket.ordered, cutoff)
		} else {
			requestDetails = append([]RequestDetail(nil), bucket.ordered...)
		}
		apiSnapshot.Models[bucket.modelName] = ModelSnapshot{
			TotalRequests:           modelStatsValue.TotalRequests,
			TotalTokens:             modelStatsValue.TotalTokens,
			TotalBillableTokens:     modelStatsValue.TotalBillableTokens,
			TotalCostUSD:            microsToUSD(modelStatsValue.TotalCostMicros),
			PricingStatus:           pricingStatusString(modelStatsValue.PricingState),
			UnpricedRequestCount:    modelStatsValue.UnpricedRequests,
			UnfinalizedRequestCount: modelStatsValue.UnfinalizedRequests,
			Details:                 requestDetails,
		}
		switch modelStatsValue.PricingState {
		case pricingStateUnpriced:
			result.UnpricedModelCount++
		case pricingStateUnfinalized:
			result.UnfinalizedModelCount++
		}
		apiSnapshots[bucket.apiName] = apiSnapshot
	}
	for apiName, apiSnapshot := range apiSnapshots {
		result.APIs[apiName] = apiSnapshot
	}

	result.RequestsByDay = make(map[string]int64, len(s.requestsByDay))
	for k, v := range s.requestsByDay {
		result.RequestsByDay[k] = v
	}

	result.RequestsByHour = make(map[string]int64, len(s.requestsByHour))
	for hour, v := range s.requestsByHour {
		key := formatHour(hour)
		result.RequestsByHour[key] = v
	}

	result.TokensByDay = make(map[string]int64, len(s.tokensByDay))
	for k, v := range s.tokensByDay {
		result.TokensByDay[k] = v
	}

	result.TokensByHour = make(map[string]int64, len(s.tokensByHour))
	for hour, v := range s.tokensByHour {
		key := formatHour(hour)
		result.TokensByHour[key] = v
	}

	result.CostByDay = make(map[string]float64, len(s.costByDayMicros))
	for k, v := range s.costByDayMicros {
		result.CostByDay[k] = microsToUSD(v)
	}

	result.CostByHour = make(map[string]float64, len(s.costByHourMicros))
	for hour, v := range s.costByHourMicros {
		key := formatHour(hour)
		result.CostByHour[key] = microsToUSD(v)
	}

	page.Snapshot = result
	page.HasMore = hasMore
	page.NextSince = nextSince
	return page
}

// filterAndSortRequestDetails sorts a model's request details by Timestamp
// ascending (stable, ties broken by original insertion order) and applies
// the Since cutoff (strictly greater-than, so a boundary record already
// synced by a caller is not re-included). DetailLimit is intentionally not
// applied here: it must be applied globally, across every bucket combined,
// by detailPageCutoff/applyDetailPageCutoff (see SnapshotPageWithOptions).
func filterAndSortRequestDetails(details []RequestDetail, since time.Time) []RequestDetail {
	// Copy before sorting: modelStatsValue.Details is the live backing slice
	// guarded by s.mu, and callers must not observe or retain a sorted
	// mutation of it.
	ordered := make([]RequestDetail, len(details))
	copy(ordered, details)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Timestamp.Before(ordered[j].Timestamp)
	})

	if since.IsZero() {
		return ordered
	}

	filtered := make([]RequestDetail, 0, len(ordered))
	for _, detail := range ordered {
		// Strictly after Since: a record exactly at the previous page's
		// NextSince cursor was already the last item returned there, so
		// re-including it here would duplicate it across pages.
		if detail.Timestamp.After(since) {
			filtered = append(filtered, detail)
		}
	}
	return filtered
}

// usageDetailBucket is a single api/model bucket's Since-filtered,
// timestamp-sorted request details, used to compute a global DetailLimit
// cutoff across all buckets combined (see detailPageCutoff).
type usageDetailBucket struct {
	apiName   string
	modelName string
	ordered   []RequestDetail
}

// detailPageCutoff computes the single global cutoff timestamp that bounds
// the *combined* (across every bucket) request-detail page at DetailLimit
// records, given totalFiltered pre-computed details across all buckets.
//
// Boundary semantics: same-timestamp records are never split across pages.
// If multiple records share the exact timestamp that would fall at the
// DetailLimit boundary, all of them are kept in this page (so the returned
// page can be slightly larger than DetailLimit when ties occur at the
// boundary) and NextSince is set to that shared timestamp so the next call
// (Since is strictly-greater-than) starts after all of them. This trades a
// small, bounded overshoot for the stronger guarantee of zero dropped and
// zero duplicated records across pages. In practice this only matters when
// two or more requests are recorded with identical RFC3339Nano timestamps
// (same wall-clock nanosecond), which is rare for real traffic but not
// impossible under high concurrency; batch-imported/merged snapshots with
// coarser timestamp precision are more likely to collide.
//
// Returns hasMore, nextSince, the cutoff timestamp, and whether a cutoff
// was applied at all (false for DetailLimit<=0 or totalFiltered<=DetailLimit,
// meaning every filtered record across all buckets fits in this page).
func detailPageCutoff(buckets []usageDetailBucket, totalFiltered, limit int) (hasMore bool, nextSince time.Time, cutoff time.Time, cutoffSet bool) {
	if limit <= 0 || totalFiltered <= limit {
		return false, time.Time{}, time.Time{}, false
	}

	// Merge every bucket's already-sorted details into one globally ordered
	// sequence to find the record at global rank `limit-1` (0-indexed). This
	// is a k-way merge over pre-sorted slices, so it is linear in
	// totalFiltered rather than requiring a full re-sort of all records.
	indices := make([]int, len(buckets))
	remaining := limit
	var last RequestDetail
	for remaining > 0 {
		bestBucket := -1
		for bi, bucket := range buckets {
			if indices[bi] >= len(bucket.ordered) {
				continue
			}
			if bestBucket == -1 || bucket.ordered[indices[bi]].Timestamp.Before(buckets[bestBucket].ordered[indices[bestBucket]].Timestamp) {
				bestBucket = bi
			}
		}
		if bestBucket == -1 {
			// Should not happen given totalFiltered > limit, but guards
			// against inconsistent bookkeeping rather than panicking.
			break
		}
		last = buckets[bestBucket].ordered[indices[bestBucket]]
		indices[bestBucket]++
		remaining--
	}

	cutoff = last.Timestamp
	cutoffSet = true

	// Determine HasMore: true if any bucket has a record strictly after the
	// cutoff timestamp (ties at the cutoff are included in this page, so
	// they do not count as "more").
	for _, bucket := range buckets {
		ordered := bucket.ordered
		if len(ordered) == 0 {
			continue
		}
		if ordered[len(ordered)-1].Timestamp.After(cutoff) {
			hasMore = true
			break
		}
	}
	if hasMore {
		nextSince = cutoff
	}
	return hasMore, nextSince, cutoff, cutoffSet
}

// applyDetailPageCutoff returns the prefix of an already Since-filtered,
// timestamp-sorted bucket whose Timestamp is <= cutoff. See detailPageCutoff
// for why ties at the boundary are kept rather than split.
func applyDetailPageCutoff(ordered []RequestDetail, cutoff time.Time) []RequestDetail {
	if len(ordered) == 0 {
		return []RequestDetail{}
	}
	end := sort.Search(len(ordered), func(i int) bool {
		return ordered[i].Timestamp.After(cutoff)
	})
	requestDetails := make([]RequestDetail, end)
	copy(requestDetails, ordered[:end])
	return requestDetails
}

type MergeResult struct {
	Added   int64 `json:"added"`
	Skipped int64 `json:"skipped"`
}

// MergeSnapshot merges an exported statistics snapshot into the current store.
// Existing data is preserved and duplicate request details are skipped.
func (s *RequestStatistics) MergeSnapshot(snapshot StatisticsSnapshot) MergeResult {
	return s.mergeSnapshot(snapshot, true)
}

func (s *RequestStatistics) mergeSnapshot(snapshot StatisticsSnapshot, persist bool) MergeResult {
	result := MergeResult{}
	if s == nil {
		return result
	}

	s.mu.Lock()

	seen := make(map[string]struct{})
	for apiName, stats := range s.apis {
		if stats == nil {
			continue
		}
		for modelName, modelStatsValue := range stats.Models {
			if modelStatsValue == nil {
				continue
			}
			for _, detail := range modelStatsValue.Details {
				seen[dedupKey(apiName, modelName, detail)] = struct{}{}
			}
		}
	}

	for apiName, apiSnapshot := range snapshot.APIs {
		apiName = strings.TrimSpace(apiName)
		if apiName == "" {
			continue
		}
		stats, ok := s.apis[apiName]
		if !ok || stats == nil {
			stats = &apiStats{Models: make(map[string]*modelStats)}
			s.apis[apiName] = stats
		} else if stats.Models == nil {
			stats.Models = make(map[string]*modelStats)
		}
		for modelName, modelSnapshot := range apiSnapshot.Models {
			modelName = strings.TrimSpace(modelName)
			if modelName == "" {
				modelName = "unknown"
			}
			for _, detail := range modelSnapshot.Details {
				detail.Tokens = normaliseTokenStats(detail.Tokens)
				if detail.LatencyMs < 0 {
					detail.LatencyMs = 0
				}
				if detail.Timestamp.IsZero() {
					detail.Timestamp = time.Now()
				}
				key := dedupKey(apiName, modelName, detail)
				if _, exists := seen[key]; exists {
					result.Skipped++
					continue
				}
				seen[key] = struct{}{}
				s.recordImported(apiName, modelName, stats, detail)
				result.Added++
			}
		}
	}

	s.mu.Unlock()
	if persist && result.Added > 0 {
		s.schedulePersistence()
	}

	return result
}

func (s *RequestStatistics) recordImported(apiName, modelName string, stats *apiStats, detail RequestDetail) {
	totalTokens := detail.Tokens.TotalTokens
	if totalTokens < 0 {
		totalTokens = 0
	}
	totalBillableTokens := detail.Tokens.BillableTokens
	if totalBillableTokens < 0 {
		totalBillableTokens = billableTokenCount(detail.Tokens)
	}

	s.totalRequests++
	if detail.Failed {
		s.failureCount++
	} else {
		s.successCount++
	}
	s.totalTokens += totalTokens
	s.totalBillableTokens += totalBillableTokens

	pricing := s.computeDetailPricing(modelName, detail.Tokens)
	if detail.CostUSD == 0 {
		detail.CostUSD = microsToUSD(pricing.CostMicros)
	}
	if detail.PricingStatus == "" && pricing.State != pricingStatePriced {
		detail.PricingStatus = string(pricing.State)
	}

	s.updateAPIStats(stats, modelName, detail, pricing)

	dayKey := detail.Timestamp.Format("2006-01-02")
	hourKey := detail.Timestamp.Hour()

	s.requestsByDay[dayKey]++
	s.requestsByHour[hourKey]++
	s.tokensByDay[dayKey] += totalTokens
	s.tokensByHour[hourKey] += totalTokens
	s.recordPricing(pricing, dayKey, hourKey)
}

func dedupKey(apiName, modelName string, detail RequestDetail) string {
	timestamp := detail.Timestamp.UTC().Format(time.RFC3339Nano)
	tokens := normaliseTokenStats(detail.Tokens)
	return fmt.Sprintf(
		"%s|%s|%s|%s|%s|%t|%d|%d|%d|%d|%d|%d|%d|%d",
		apiName,
		modelName,
		timestamp,
		detail.Source,
		detail.AuthIndex,
		detail.Failed,
		tokens.InputTokens,
		tokens.OutputTokens,
		tokens.ReasoningTokens,
		tokens.CachedTokens,
		tokens.CacheReadTokens,
		tokens.CacheWriteTokens,
		tokens.TotalTokens,
		tokens.BillableTokens,
	)
}

func (s *RequestStatistics) recordPricing(pricing pricingTotals, dayKey string, hourKey int) {
	s.totalCostMicros += pricing.CostMicros
	s.unpricedRequests += pricing.Unpriced
	s.unfinalizedRequests += pricing.Unfinalized
	s.costByDayMicros[dayKey] += pricing.CostMicros
	s.costByHourMicros[hourKey] += pricing.CostMicros
}

// PricingSnapshot returns the current pricing catalog and detected models based on observed usage.
func (s *RequestStatistics) PricingSnapshot() PricingSnapshot {
	if s == nil {
		return defaultPricingCatalog.Snapshot(nil)
	}

	s.mu.RLock()
	observed := make(map[string]string)
	for _, apiStatsValue := range s.apis {
		if apiStatsValue == nil {
			continue
		}
		for modelName, modelStatsValue := range apiStatsValue.Models {
			if modelStatsValue == nil || len(modelStatsValue.Details) == 0 {
				continue
			}
			if _, exists := observed[modelName]; !exists {
				observed[modelName] = s.computeDetailPricing(modelName, TokenStats{}).UniqueModelName
			}
		}
	}
	catalog := s.pricingCatalog()
	s.mu.RUnlock()

	observationList := make([]modelObservation, 0, len(observed))
	for observedModel, canonical := range observed {
		observationList = append(observationList, modelObservation{
			Observed:  observedModel,
			Canonical: canonical,
		})
	}
	return catalog.Snapshot(observationList)
}

// RefreshPricingCatalog refreshes official prices and recalculates historical usage costs.
func (s *RequestStatistics) RefreshPricingCatalog(ctx context.Context) error {
	catalog := s.pricingCatalog()
	err := catalog.RefreshOfficial(ctx)
	s.RecalculatePricing()
	return err
}

// PutPricingOverride stores a manual pricing override and recalculates historical usage costs.
func (s *RequestStatistics) PutPricingOverride(model string, override PricingModel) (PricingModel, error) {
	entry, err := s.pricingCatalog().PutOverride(model, override)
	if err != nil {
		return PricingModel{}, err
	}
	s.RecalculatePricing()
	return entry, nil
}

// DeletePricingOverride removes a manual pricing override and recalculates historical usage costs.
func (s *RequestStatistics) DeletePricingOverride(model string) bool {
	deleted := s.pricingCatalog().DeleteOverride(model)
	if deleted {
		s.RecalculatePricing()
	}
	return deleted
}

// RecalculatePricing recomputes all cost fields from the current pricing catalog.
func (s *RequestStatistics) RecalculatePricing() {
	if s == nil {
		return
	}

	s.mu.Lock()
	s.recalculatePricingLocked()
	s.mu.Unlock()
	s.schedulePersistence()
}

func (s *RequestStatistics) recalculatePricingLocked() {
	s.totalRequests = 0
	s.successCount = 0
	s.failureCount = 0
	s.totalTokens = 0
	s.totalBillableTokens = 0
	s.totalCostMicros = 0
	s.unpricedRequests = 0
	s.unfinalizedRequests = 0

	s.requestsByDay = make(map[string]int64)
	s.requestsByHour = make(map[int]int64)
	s.tokensByDay = make(map[string]int64)
	s.tokensByHour = make(map[int]int64)
	s.costByDayMicros = make(map[string]int64)
	s.costByHourMicros = make(map[int]int64)

	for _, apiStatsValue := range s.apis {
		if apiStatsValue == nil {
			continue
		}
		apiStatsValue.TotalRequests = 0
		apiStatsValue.TotalTokens = 0
		apiStatsValue.TotalBillableTokens = 0
		apiStatsValue.TotalCostMicros = 0
		apiStatsValue.UnpricedRequests = 0
		apiStatsValue.UnfinalizedRequests = 0
		if apiStatsValue.Models == nil {
			apiStatsValue.Models = make(map[string]*modelStats)
		}

		for modelName, modelStatsValue := range apiStatsValue.Models {
			if modelStatsValue == nil {
				modelStatsValue = &modelStats{}
				apiStatsValue.Models[modelName] = modelStatsValue
			}
			modelStatsValue.TotalRequests = 0
			modelStatsValue.TotalTokens = 0
			modelStatsValue.TotalBillableTokens = 0
			modelStatsValue.TotalCostMicros = 0
			modelStatsValue.UnpricedRequests = 0
			modelStatsValue.UnfinalizedRequests = 0
			modelStatsValue.PricingState = ""

			for idx := range modelStatsValue.Details {
				detail := modelStatsValue.Details[idx]
				detail.Tokens = normaliseTokenStats(detail.Tokens)
				pricing := s.computeDetailPricing(modelName, detail.Tokens)
				detail.CostUSD = microsToUSD(pricing.CostMicros)
				detail.PricingStatus = pricingStatusString(pricing.State)
				modelStatsValue.Details[idx] = detail

				totalTokens := detail.Tokens.TotalTokens
				totalBillableTokens := detail.Tokens.BillableTokens
				s.totalRequests++
				if detail.Failed {
					s.failureCount++
				} else {
					s.successCount++
				}
				s.totalTokens += totalTokens
				s.totalBillableTokens += totalBillableTokens

				apiStatsValue.TotalRequests++
				apiStatsValue.TotalTokens += totalTokens
				apiStatsValue.TotalBillableTokens += totalBillableTokens
				apiStatsValue.TotalCostMicros += pricing.CostMicros
				apiStatsValue.UnpricedRequests += pricing.Unpriced
				apiStatsValue.UnfinalizedRequests += pricing.Unfinalized

				modelStatsValue.TotalRequests++
				modelStatsValue.TotalTokens += totalTokens
				modelStatsValue.TotalBillableTokens += totalBillableTokens
				modelStatsValue.TotalCostMicros += pricing.CostMicros
				modelStatsValue.UnpricedRequests += pricing.Unpriced
				modelStatsValue.UnfinalizedRequests += pricing.Unfinalized
				modelStatsValue.PricingState = pricingStateForCounts(
					modelStatsValue.TotalRequests,
					modelStatsValue.UnpricedRequests,
					modelStatsValue.UnfinalizedRequests,
				)

				dayKey := detail.Timestamp.Format("2006-01-02")
				hourKey := detail.Timestamp.Hour()
				s.requestsByDay[dayKey]++
				s.requestsByHour[hourKey]++
				s.tokensByDay[dayKey] += totalTokens
				s.tokensByHour[hourKey] += totalTokens
				s.recordPricing(pricing, dayKey, hourKey)
			}
		}
	}
}

func (s *RequestStatistics) computeDetailPricing(model string, tokens TokenStats) pricingTotals {
	return s.pricingCatalog().ComputeDetailPricing(model, tokens)
}

func (s *RequestStatistics) pricingCatalog() *PricingCatalogManager {
	if s == nil || s.catalog == nil {
		return defaultPricingCatalog
	}
	return s.catalog
}

func pricingStateForCounts(totalRequests, unpricedRequests, unfinalizedRequests int64) pricingState {
	if totalRequests <= 0 {
		return ""
	}
	if unpricedRequests == 0 && unfinalizedRequests == 0 {
		return pricingStatePriced
	}
	if unpricedRequests == totalRequests && unfinalizedRequests == 0 {
		return pricingStateUnpriced
	}
	if unfinalizedRequests == totalRequests && unpricedRequests == 0 {
		return pricingStateUnfinalized
	}
	return pricingStatePartial
}

func pricingStatusString(state pricingState) string {
	if state == "" || state == pricingStatePriced {
		return ""
	}
	return string(state)
}

func resolveAPIIdentifier(ctx context.Context, record coreusage.Record) string {
	if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil {
			path := ginCtx.FullPath()
			if path == "" && ginCtx.Request != nil {
				path = ginCtx.Request.URL.Path
			}
			method := ""
			if ginCtx.Request != nil {
				method = ginCtx.Request.Method
			}
			if path != "" {
				if method != "" {
					return method + " " + path
				}
				return path
			}
		}
	}
	if record.Provider != "" {
		return record.Provider
	}
	return "unknown"
}

func resolveSuccess(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil {
		return true
	}
	status := ginCtx.Writer.Status()
	if status == 0 {
		return true
	}
	return status < httpStatusBadRequest
}

const httpStatusBadRequest = 400

func normaliseDetail(detail coreusage.Detail) TokenStats {
	tokens := TokenStats{
		InputTokens:      detail.InputTokens,
		OutputTokens:     detail.OutputTokens,
		ReasoningTokens:  detail.ReasoningTokens,
		CachedTokens:     detail.CachedTokens,
		CacheReadTokens:  detail.CacheReadTokens,
		CacheWriteTokens: detail.CacheWriteTokens,
		TotalTokens:      detail.TotalTokens,
	}
	if tokens.CacheReadTokens == 0 && tokens.CachedTokens > 0 {
		tokens.CacheReadTokens = tokens.CachedTokens
	}
	if tokens.CachedTokens == 0 && tokens.CacheReadTokens > 0 {
		tokens.CachedTokens = tokens.CacheReadTokens
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	if tokens.BillableTokens == 0 {
		tokens.BillableTokens = billableTokenCount(tokens)
	}
	return tokens
}

func normaliseTokenStats(tokens TokenStats) TokenStats {
	if tokens.CacheReadTokens == 0 && tokens.CachedTokens > 0 {
		tokens.CacheReadTokens = tokens.CachedTokens
	}
	if tokens.CachedTokens == 0 && tokens.CacheReadTokens > 0 {
		tokens.CachedTokens = tokens.CacheReadTokens
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	}
	if tokens.BillableTokens == 0 {
		tokens.BillableTokens = billableTokenCount(tokens)
	}
	return tokens
}

func billableTokenCount(tokens TokenStats) int64 {
	total := tokens.TotalTokens
	if total == 0 {
		total = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	}
	cacheRead := maxInt64(tokens.CacheReadTokens, tokens.CachedTokens)
	return total + cacheRead + tokens.CacheWriteTokens
}

func normaliseLatency(latency time.Duration) int64 {
	if latency <= 0 {
		return 0
	}
	return latency.Milliseconds()
}

func formatHour(hour int) string {
	if hour < 0 {
		hour = 0
	}
	hour = hour % 24
	return fmt.Sprintf("%02d", hour)
}
