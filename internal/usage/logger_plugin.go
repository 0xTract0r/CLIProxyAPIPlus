// Package usage provides usage tracking and logging functionality for the CLI Proxy API server.
// It includes plugins for monitoring API usage, token consumption, and other metrics
// to help with observability and billing purposes.
package usage

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
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
	TotalCostMicros     int64
	UnpricedRequests    int64
	UnfinalizedRequests int64
	Models              map[string]*modelStats
}

// modelStats holds aggregated metrics for a specific model within an API.
type modelStats struct {
	TotalRequests       int64
	TotalTokens         int64
	TotalCostMicros     int64
	UnpricedRequests    int64
	UnfinalizedRequests int64
	PricingState        pricingState
	Details             []RequestDetail
}

// RequestDetail stores the timestamp, latency, and token usage for a single request.
type RequestDetail struct {
	Timestamp     time.Time  `json:"timestamp"`
	LatencyMs     int64      `json:"latency_ms"`
	Source        string     `json:"source"`
	AuthIndex     string     `json:"auth_index"`
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
}

// StatisticsSnapshot represents an immutable view of the aggregated metrics.
type StatisticsSnapshot struct {
	TotalRequests           int64   `json:"total_requests"`
	SuccessCount            int64   `json:"success_count"`
	FailureCount            int64   `json:"failure_count"`
	TotalTokens             int64   `json:"total_tokens"`
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
	TotalCostUSD            float64         `json:"total_cost_usd"`
	PricingStatus           string          `json:"pricing_status,omitempty"`
	UnpricedRequestCount    int64           `json:"unpriced_request_count,omitempty"`
	UnfinalizedRequestCount int64           `json:"unfinalized_request_count,omitempty"`
	Details                 []RequestDetail `json:"details"`
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
	result := StatisticsSnapshot{}
	if s == nil {
		return result
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	result.TotalRequests = s.totalRequests
	result.SuccessCount = s.successCount
	result.FailureCount = s.failureCount
	result.TotalTokens = s.totalTokens
	result.TotalCostUSD = microsToUSD(s.totalCostMicros)
	result.UnpricedRequestCount = s.unpricedRequests
	result.UnfinalizedRequestCount = s.unfinalizedRequests
	result.PricingStatus = pricingStatusString(
		pricingStateForCounts(result.TotalRequests, result.UnpricedRequestCount, result.UnfinalizedRequestCount),
	)

	result.APIs = make(map[string]APISnapshot, len(s.apis))
	for apiName, stats := range s.apis {
		apiSnapshot := APISnapshot{
			TotalRequests:           stats.TotalRequests,
			TotalTokens:             stats.TotalTokens,
			TotalCostUSD:            microsToUSD(stats.TotalCostMicros),
			PricingStatus:           pricingStatusString(pricingStateForCounts(stats.TotalRequests, stats.UnpricedRequests, stats.UnfinalizedRequests)),
			UnpricedRequestCount:    stats.UnpricedRequests,
			UnfinalizedRequestCount: stats.UnfinalizedRequests,
			Models:                  make(map[string]ModelSnapshot, len(stats.Models)),
		}
		for modelName, modelStatsValue := range stats.Models {
			requestDetails := make([]RequestDetail, len(modelStatsValue.Details))
			copy(requestDetails, modelStatsValue.Details)
			apiSnapshot.Models[modelName] = ModelSnapshot{
				TotalRequests:           modelStatsValue.TotalRequests,
				TotalTokens:             modelStatsValue.TotalTokens,
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
		}
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

	return result
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

	s.totalRequests++
	if detail.Failed {
		s.failureCount++
	} else {
		s.successCount++
	}
	s.totalTokens += totalTokens

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
		"%s|%s|%s|%s|%s|%t|%d|%d|%d|%d|%d|%d|%d",
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
				s.totalRequests++
				if detail.Failed {
					s.failureCount++
				} else {
					s.successCount++
				}
				s.totalTokens += totalTokens

				apiStatsValue.TotalRequests++
				apiStatsValue.TotalTokens += totalTokens
				apiStatsValue.TotalCostMicros += pricing.CostMicros
				apiStatsValue.UnpricedRequests += pricing.Unpriced
				apiStatsValue.UnfinalizedRequests += pricing.Unfinalized

				modelStatsValue.TotalRequests++
				modelStatsValue.TotalTokens += totalTokens
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
		tokens.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens + detail.CacheReadTokens + detail.CacheWriteTokens
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens + detail.CachedTokens
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
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens + tokens.CacheReadTokens + tokens.CacheWriteTokens
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens + tokens.CachedTokens
	}
	return tokens
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
