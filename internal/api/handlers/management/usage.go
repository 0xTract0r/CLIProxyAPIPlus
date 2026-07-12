package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
)

type usageQueueRecord []byte

func (r usageQueueRecord) MarshalJSON() ([]byte, error) {
	if json.Valid(r) {
		return append([]byte(nil), r...), nil
	}
	return json.Marshal(string(r))
}

type usageExportPayload struct {
	Version    int                      `json:"version"`
	ExportedAt time.Time                `json:"exported_at"`
	Usage      usage.StatisticsSnapshot `json:"usage"`
	// HasMore indicates the export was windowed (since/limit) and truncated
	// request details remain beyond NextSince, across the combined page (not
	// per-model). Absent/false for a full, unwindowed export (backward
	// compatible default).
	HasMore bool `json:"has_more,omitempty"`
	// NextSince is the cursor callers should pass as `since` on the next
	// export call to continue pulling remaining details in stable order.
	// RFC3339Nano precision to avoid losing sub-second records at the
	// window boundary. Empty when HasMore is false.
	NextSince string `json:"next_since,omitempty"`
}

type usageImportPayload struct {
	Version int                      `json:"version"`
	Usage   usage.StatisticsSnapshot `json:"usage"`
}

type pricingPayload struct {
	Pricing usage.PricingSnapshot `json:"pricing"`
}

type pricingOverridePayload struct {
	Model                 string  `json:"model"`
	DisplayName           string  `json:"display_name"`
	InputUSDPerMTok       float64 `json:"input_usd_per_mtok"`
	CachedInputUSDPerMTok float64 `json:"cached_input_usd_per_mtok"`
	OutputUSDPerMTok      float64 `json:"output_usd_per_mtok"`
	CacheWriteUSDPerMTok  float64 `json:"cache_write_usd_per_mtok"`
}

const maxUsageDetailLimit = 10000

// GetUsageStatistics returns the in-memory request statistics snapshot.
func (h *Handler) GetUsageStatistics(c *gin.Context) {
	options, errOptions := parseUsageSnapshotOptions(c)
	if errOptions != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errOptions.Error()})
		return
	}
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.SnapshotWithOptions(options)
	}
	c.JSON(http.StatusOK, gin.H{
		"usage":           snapshot,
		"failed_requests": snapshot.FailureCount,
	})
}

func parseUsageSnapshotOptions(c *gin.Context) (usage.SnapshotOptions, error) {
	options := usage.SnapshotOptions{}
	if c == nil {
		return options, nil
	}
	if raw := strings.TrimSpace(c.Query("include_details")); raw != "" {
		includeDetails, err := strconv.ParseBool(raw)
		if err != nil {
			return options, fmt.Errorf("include_details must be a boolean")
		}
		options.ExcludeDetails = !includeDetails
	}
	if raw := strings.TrimSpace(c.Query("since")); raw != "" {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return options, fmt.Errorf("since must be RFC3339")
		}
		options.Since = since
	}
	if raw := strings.TrimSpace(c.Query("detail_limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			return options, fmt.Errorf("detail_limit must be a positive integer")
		}
		if limit > maxUsageDetailLimit {
			limit = maxUsageDetailLimit
		}
		options.DetailLimit = limit
	}
	return options, nil
}

// ExportUsageStatistics returns a usage snapshot for backup/migration or
// incremental sync. Without query parameters it returns the full snapshot
// (backward compatible). With `since` and/or `limit`, it returns a windowed
// page of request details plus `has_more`/`next_since` pagination cursors so
// callers (e.g. cpamp) can pull large histories in stable, gap-free batches.
func (h *Handler) ExportUsageStatistics(c *gin.Context) {
	options, err := parseUsageExportOptions(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var page usage.SnapshotPage
	if h != nil && h.usageStats != nil {
		page = h.usageStats.SnapshotPageWithOptions(options)
	}

	payload := usageExportPayload{
		Version:    2,
		ExportedAt: time.Now().UTC(),
		Usage:      page.Snapshot,
		HasMore:    page.HasMore,
	}
	if page.HasMore && !page.NextSince.IsZero() {
		payload.NextSince = page.NextSince.UTC().Format(time.RFC3339Nano)
	}
	c.JSON(http.StatusOK, payload)
}

// parseUsageExportOptions parses the `since` and `limit` query parameters for
// /usage/export. `since` accepts RFC3339 (e.g. 2026-01-01T00:00:00Z) or a
// Unix timestamp in milliseconds. `limit` is the maximum number of request
// details to return in total across every api/model bucket combined (0 or
// absent means unlimited, i.e. full export); it is a global cap on the
// payload, not a per-model cap, so the response size stays bounded even when
// a snapshot spans many api keys and models. Unlike parseUsageSnapshotOptions
// (used by the live statistics endpoint), no upper bound is imposed on
// `limit` here: export callers explicitly control batch size for their own
// sync cadence.
func parseUsageExportOptions(c *gin.Context) (usage.SnapshotOptions, error) {
	options := usage.SnapshotOptions{}
	if c == nil {
		return options, nil
	}
	if raw := strings.TrimSpace(c.Query("since")); raw != "" {
		since, err := parseUsageExportSince(raw)
		if err != nil {
			return options, err
		}
		options.Since = since
	}
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 0 {
			return options, fmt.Errorf("limit must be a non-negative integer")
		}
		options.DetailLimit = limit
	}
	return options, nil
}

// parseUsageExportSince parses `since` as RFC3339 first, then falls back to a
// Unix millisecond timestamp.
func parseUsageExportSince(raw string) (time.Time, error) {
	if since, err := time.Parse(time.RFC3339, raw); err == nil {
		return since, nil
	}
	millis, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("since must be RFC3339 or a unix millisecond timestamp")
	}
	return time.UnixMilli(millis).UTC(), nil
}

// ImportUsageStatistics merges a previously exported usage snapshot into memory.
func (h *Handler) ImportUsageStatistics(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}

	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	var payload usageImportPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if payload.Version != 0 && payload.Version != 1 && payload.Version != 2 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported version"})
		return
	}

	result := h.usageStats.MergeSnapshot(payload.Usage)
	snapshot := h.usageStats.Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"added":           result.Added,
		"skipped":         result.Skipped,
		"total_requests":  snapshot.TotalRequests,
		"failed_requests": snapshot.FailureCount,
	})
}

// GetUsageQueue pops queued usage records from the usage queue.
func (h *Handler) GetUsageQueue(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	count, errCount := parseUsageQueueCount(c.Query("count"))
	if errCount != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errCount.Error()})
		return
	}

	items := redisqueue.PopOldest(count)
	records := make([]usageQueueRecord, 0, len(items))
	for _, item := range items {
		records = append(records, usageQueueRecord(append([]byte(nil), item...)))
	}

	c.JSON(http.StatusOK, records)
}

func parseUsageQueueCount(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 1, nil
	}
	count, errCount := strconv.Atoi(value)
	if errCount != nil || count <= 0 {
		return 0, errors.New("count must be a positive integer")
	}
	return count, nil
}

// GetUsagePricing returns the current pricing catalog and observed models.
func (h *Handler) GetUsagePricing(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}
	c.JSON(http.StatusOK, pricingPayload{
		Pricing: h.usageStats.PricingSnapshot(),
	})
}

// RefreshUsagePricing refreshes official pricing sources and recalculates historical costs.
func (h *Handler) RefreshUsagePricing(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}
	err := h.usageStats.RefreshPricingCatalog(context.Background())
	payload := pricingPayload{Pricing: h.usageStats.PricingSnapshot()}
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"pricing": payload.Pricing,
			"warning": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, payload)
}

// PutUsagePricingOverride stores a manual pricing override and recalculates historical costs.
func (h *Handler) PutUsagePricingOverride(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}

	model := c.Param("model")
	var payload pricingOverridePayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if payload.Model != "" && usage.NormalizeCanonicalModelID(payload.Model) != usage.NormalizeCanonicalModelID(model) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body model does not match path model"})
		return
	}

	if _, err := h.usageStats.PutPricingOverride(model, usage.PricingModel{
		DisplayName:           payload.DisplayName,
		InputUSDPerMTok:       payload.InputUSDPerMTok,
		CachedInputUSDPerMTok: payload.CachedInputUSDPerMTok,
		OutputUSDPerMTok:      payload.OutputUSDPerMTok,
		CacheWriteUSDPerMTok:  payload.CacheWriteUSDPerMTok,
	}); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, pricingPayload{
		Pricing: h.usageStats.PricingSnapshot(),
	})
}

// DeleteUsagePricingOverride removes a manual pricing override and recalculates historical costs.
func (h *Handler) DeleteUsagePricingOverride(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}
	if !h.usageStats.DeletePricingOverride(c.Param("model")) {
		c.JSON(http.StatusNotFound, gin.H{"error": "pricing override not found"})
		return
	}
	c.JSON(http.StatusOK, pricingPayload{
		Pricing: h.usageStats.PricingSnapshot(),
	})
}
