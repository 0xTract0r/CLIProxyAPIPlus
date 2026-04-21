package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type pricingState string

const (
	pricingStatePriced      pricingState = "priced"
	pricingStateUnpriced    pricingState = "unpriced"
	pricingStateUnfinalized pricingState = "unfinalized"
	pricingStatePartial     pricingState = "partial"
)

const pricingCatalogFileVersion = 1

const (
	pricingSourceBuiltin  = "builtin"
	pricingSourceOfficial = "official"
	pricingSourceOverride = "override"
	pricingSourceNone     = "none"
)

const (
	pricingSourceStatusOK    = "ok"
	pricingSourceStatusError = "error"
)

const (
	defaultOpenAIPricingURL    = "https://developers.openai.com/api/docs/pricing"
	defaultAnthropicPricingURL = "https://www.anthropic.com/pricing"
	defaultPricingUserAgent    = "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"
)

var (
	openAIPricingRowPattern = regexp.MustCompile(`\[\[0,"([^"]+)"\],\[0,([^\]]+)\],\[0,([^\]]+)\],\[0,([^\]]+)\]\]`)
	anthropicTitlePattern   = regexp.MustCompile(`>\s*([^<]+?)\s*<`)
	anthropicValuePattern   = regexp.MustCompile(`data-value="([^"]+)"`)
	trailingDatePattern     = regexp.MustCompile(`(?:-|_)(20\d{6}|\d{8})$`)
	trailingSnapshotPattern = regexp.MustCompile(`(?:-|_)(snapshot|latest|thinking|model)$`)
	parentheticalSuffix     = regexp.MustCompile(`\s*\([^)]*\)`)
	separatorPattern        = regexp.MustCompile(`[^a-z0-9]+`)
)

type modelPrice struct {
	InputMicros      int64
	OutputMicros     int64
	CacheReadMicros  int64
	CacheWriteMicros int64
}

type pricingTotals struct {
	CostMicros      int64
	State           pricingState
	Unpriced        int64
	Unfinalized     int64
	UniqueModelName string
	Source          string
	Price           PricingModel
}

type PricingModel struct {
	Model                 string       `json:"model"`
	DisplayName           string       `json:"display_name,omitempty"`
	InputUSDPerMTok       float64      `json:"input_usd_per_mtok,omitempty"`
	CachedInputUSDPerMTok float64      `json:"cached_input_usd_per_mtok,omitempty"`
	OutputUSDPerMTok      float64      `json:"output_usd_per_mtok,omitempty"`
	CacheWriteUSDPerMTok  float64      `json:"cache_write_usd_per_mtok,omitempty"`
	Source                string       `json:"source,omitempty"`
	CanonicalModel        string       `json:"canonical_model,omitempty"`
	PricingStatus         pricingState `json:"-"`
	SourceID              string       `json:"-"`
}

type PricingSourceInfo struct {
	ID              string    `json:"id"`
	Label           string    `json:"label"`
	URL             string    `json:"url"`
	Status          string    `json:"status,omitempty"`
	Message         string    `json:"message,omitempty"`
	LastRefreshedAt time.Time `json:"last_refreshed_at,omitempty"`
	ModelCount      int       `json:"model_count,omitempty"`
}

type PricingOfficialSnapshot struct {
	LastRefreshedAt time.Time           `json:"last_refreshed_at,omitempty"`
	PersistedAt     time.Time           `json:"persisted_at,omitempty"`
	Sources         []PricingSourceInfo `json:"sources"`
}

type DetectedPricingModel struct {
	ObservedModel         string  `json:"observed_model"`
	CanonicalModel        string  `json:"canonical_model,omitempty"`
	PricingStatus         string  `json:"pricing_status"`
	Source                string  `json:"source"`
	Model                 string  `json:"model,omitempty"`
	DisplayName           string  `json:"display_name,omitempty"`
	InputUSDPerMTok       float64 `json:"input_usd_per_mtok,omitempty"`
	CachedInputUSDPerMTok float64 `json:"cached_input_usd_per_mtok,omitempty"`
	OutputUSDPerMTok      float64 `json:"output_usd_per_mtok,omitempty"`
	CacheWriteUSDPerMTok  float64 `json:"cache_write_usd_per_mtok,omitempty"`
}

type PricingSnapshot struct {
	Official       PricingOfficialSnapshot `json:"official"`
	Models         map[string]PricingModel `json:"models"`
	Overrides      map[string]PricingModel `json:"overrides"`
	DetectedModels []DetectedPricingModel  `json:"detected_models"`
}

type pricingCatalogFile struct {
	Version         int                          `json:"version"`
	PersistedAt     time.Time                    `json:"persisted_at"`
	LastRefreshedAt time.Time                    `json:"last_refreshed_at,omitempty"`
	OfficialModels  map[string]PricingModel      `json:"official_models,omitempty"`
	Overrides       map[string]PricingModel      `json:"overrides,omitempty"`
	Sources         map[string]PricingSourceInfo `json:"sources,omitempty"`
}

type pricingFetcher struct {
	ID    string
	Label string
	URL   string
	Fetch func(context.Context, *http.Client, string) (map[string]PricingModel, error)
}

type PricingCatalogManager struct {
	mu sync.RWMutex

	filePath         string
	persistedAt      time.Time
	lastRefreshedAt  time.Time
	officialModels   map[string]PricingModel
	overrides        map[string]PricingModel
	sources          map[string]PricingSourceInfo
	aliasToCanonical map[string]string

	httpClient *http.Client
	fetchers   []pricingFetcher
}

type modelObservation struct {
	Observed  string
	Canonical string
}

var defaultPricingCatalog = NewPricingCatalogManager()

func NewPricingCatalogManager() *PricingCatalogManager {
	manager := &PricingCatalogManager{
		officialModels: make(map[string]PricingModel),
		overrides:      make(map[string]PricingModel),
		sources:        make(map[string]PricingSourceInfo),
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
	}
	manager.fetchers = []pricingFetcher{
		{
			ID:    "openai",
			Label: "OpenAI Pricing",
			URL:   defaultOpenAIPricingURL,
			Fetch: fetchOpenAIPricing,
		},
		{
			ID:    "anthropic",
			Label: "Anthropic Pricing",
			URL:   defaultAnthropicPricingURL,
			Fetch: fetchAnthropicPricing,
		},
	}
	manager.rebuildAliasIndexLocked()
	return manager
}

func GetDefaultPricingCatalog() *PricingCatalogManager {
	return defaultPricingCatalog
}

// NormalizeCanonicalModelID exposes model canonicalization for handlers and tests.
func NormalizeCanonicalModelID(model string) string {
	return normalizeCanonicalModelID(model)
}

func ConfigureDefaultPricingCatalogPersistence(path string) error {
	if err := defaultPricingCatalog.SetPersistencePath(path); err != nil {
		return err
	}
	// 默认统计仓库与默认 pricing catalog 绑定，恢复 catalog 后需要同步重算历史 cost。
	defaultRequestStatistics.RecalculatePricing()
	return nil
}

func (m *PricingCatalogManager) SetHTTPClient(client *http.Client) {
	if m == nil || client == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.httpClient = client
}

func (m *PricingCatalogManager) SetFetchers(fetchers []pricingFetcher) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fetchers = append([]pricingFetcher(nil), fetchers...)
	m.ensureSourceMetadataLocked()
}

func (m *PricingCatalogManager) SetPersistencePath(path string) error {
	if m == nil {
		return nil
	}

	cleaned := strings.TrimSpace(path)
	if cleaned != "" {
		cleaned = filepath.Clean(cleaned)
		if !filepath.IsAbs(cleaned) {
			if abs, err := filepath.Abs(cleaned); err == nil {
				cleaned = abs
			}
		}
	}

	m.mu.Lock()
	m.filePath = cleaned
	m.mu.Unlock()

	if cleaned == "" {
		return nil
	}
	return m.LoadFromPersistence()
}

func (m *PricingCatalogManager) SaveToPersistence() error {
	if m == nil {
		return nil
	}
	path := m.persistencePath()
	if path == "" {
		return nil
	}

	m.mu.RLock()
	payload := pricingCatalogFile{
		Version:         pricingCatalogFileVersion,
		PersistedAt:     time.Now().UTC(),
		LastRefreshedAt: m.lastRefreshedAt,
		OfficialModels:  clonePricingModelMap(m.officialModels),
		Overrides:       clonePricingModelMap(m.overrides),
		Sources:         clonePricingSourceMap(m.sources),
	}
	m.mu.RUnlock()

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}

	m.mu.Lock()
	m.persistedAt = payload.PersistedAt
	m.mu.Unlock()
	return nil
}

func (m *PricingCatalogManager) LoadFromPersistence() error {
	if m == nil {
		return nil
	}
	path := m.persistencePath()
	if path == "" {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	var payload pricingCatalogFile
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	if payload.Version != 0 && payload.Version != pricingCatalogFileVersion {
		return fmt.Errorf("unsupported pricing catalog version %d", payload.Version)
	}

	m.mu.Lock()
	m.persistedAt = payload.PersistedAt
	m.lastRefreshedAt = payload.LastRefreshedAt
	m.officialModels = canonicalizePricingModelMap(payload.OfficialModels, pricingSourceOfficial)
	m.overrides = canonicalizePricingModelMap(payload.Overrides, pricingSourceOverride)
	m.sources = clonePricingSourceMap(payload.Sources)
	m.ensureSourceMetadataLocked()
	m.rebuildAliasIndexLocked()
	m.mu.Unlock()
	return nil
}

func (m *PricingCatalogManager) persistencePath() string {
	if m == nil {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.filePath
}

func (m *PricingCatalogManager) PutOverride(model string, override PricingModel) (PricingModel, error) {
	if m == nil {
		return PricingModel{}, errors.New("pricing catalog unavailable")
	}
	canonical := normalizeCanonicalModelID(model)
	if canonical == "" {
		return PricingModel{}, errors.New("invalid model")
	}
	if override.InputUSDPerMTok <= 0 || override.OutputUSDPerMTok <= 0 {
		return PricingModel{}, errors.New("input_usd_per_mtok and output_usd_per_mtok must be positive")
	}
	if override.CachedInputUSDPerMTok < 0 || override.CacheWriteUSDPerMTok < 0 {
		return PricingModel{}, errors.New("cached_input_usd_per_mtok and cache_write_usd_per_mtok must be non-negative")
	}

	entry := normalizePricingModel(override, canonical, pricingSourceOverride)
	entry.PricingStatus = pricingStatePriced

	m.mu.Lock()
	m.overrides[canonical] = entry
	m.rebuildAliasIndexLocked()
	m.mu.Unlock()

	if err := m.SaveToPersistence(); err != nil {
		return PricingModel{}, err
	}
	return entry, nil
}

func (m *PricingCatalogManager) DeleteOverride(model string) bool {
	if m == nil {
		return false
	}
	canonical := normalizeCanonicalModelID(model)
	if canonical == "" {
		return false
	}

	m.mu.Lock()
	_, existed := m.overrides[canonical]
	if existed {
		delete(m.overrides, canonical)
		m.rebuildAliasIndexLocked()
	}
	m.mu.Unlock()
	if existed {
		_ = m.SaveToPersistence()
	}
	return existed
}

func (m *PricingCatalogManager) RefreshOfficial(ctx context.Context) error {
	if m == nil {
		return errors.New("pricing catalog unavailable")
	}

	m.mu.RLock()
	client := m.httpClient
	fetchers := append([]pricingFetcher(nil), m.fetchers...)
	currentOfficial := clonePricingModelMap(m.officialModels)
	currentSources := clonePricingSourceMap(m.sources)
	m.mu.RUnlock()

	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}

	now := time.Now().UTC()
	successCount := 0
	var refreshErrors []string

	for _, fetcher := range fetchers {
		sourceInfo := currentSources[fetcher.ID]
		sourceInfo.ID = fetcher.ID
		sourceInfo.Label = fetcher.Label
		sourceInfo.URL = fetcher.URL

		models, err := fetcher.Fetch(ctx, client, fetcher.URL)
		if err != nil {
			sourceInfo.Status = pricingSourceStatusError
			sourceInfo.Message = err.Error()
			currentSources[fetcher.ID] = sourceInfo
			refreshErrors = append(refreshErrors, fetcher.ID+": "+err.Error())
			continue
		}

		for key, model := range currentOfficial {
			if model.SourceID == fetcher.ID {
				delete(currentOfficial, key)
			}
		}
		for canonical, model := range models {
			model = normalizePricingModel(model, canonical, pricingSourceOfficial)
			model.SourceID = fetcher.ID
			model.PricingStatus = pricingStatePriced
			currentOfficial[canonical] = model
		}

		sourceInfo.Status = pricingSourceStatusOK
		sourceInfo.Message = ""
		sourceInfo.ModelCount = len(models)
		sourceInfo.LastRefreshedAt = now
		currentSources[fetcher.ID] = sourceInfo
		successCount++
	}

	if successCount == 0 {
		return fmt.Errorf("pricing refresh failed: %s", strings.Join(refreshErrors, "; "))
	}

	m.mu.Lock()
	m.officialModels = currentOfficial
	m.sources = currentSources
	m.lastRefreshedAt = now
	m.ensureSourceMetadataLocked()
	m.rebuildAliasIndexLocked()
	m.mu.Unlock()

	if err := m.SaveToPersistence(); err != nil {
		return err
	}
	if len(refreshErrors) > 0 {
		return fmt.Errorf("pricing refresh partial failure: %s", strings.Join(refreshErrors, "; "))
	}
	return nil
}

func (m *PricingCatalogManager) Snapshot(observations []modelObservation) PricingSnapshot {
	if m == nil {
		return PricingSnapshot{
			Official:  PricingOfficialSnapshot{Sources: []PricingSourceInfo{}},
			Models:    map[string]PricingModel{},
			Overrides: map[string]PricingModel{},
		}
	}

	m.mu.RLock()
	effective := m.effectiveModelsLocked()
	overrides := clonePricingModelMap(m.overrides)
	official := PricingOfficialSnapshot{
		LastRefreshedAt: m.lastRefreshedAt,
		PersistedAt:     m.persistedAt,
		Sources:         sortedPricingSourcesLocked(m.sources),
	}
	m.mu.RUnlock()

	snapshot := PricingSnapshot{
		Official:  official,
		Models:    effective,
		Overrides: overrides,
	}
	for _, observation := range observations {
		pricing := m.ComputeDetailPricing(observation.Observed, TokenStats{})
		detected := DetectedPricingModel{
			ObservedModel:  observation.Observed,
			CanonicalModel: pricing.UniqueModelName,
			PricingStatus:  string(pricing.State),
			Source:         pricing.Source,
		}
		if detected.Source == "" {
			detected.Source = pricingSourceNone
		}
		if pricing.Price.Model != "" {
			detected.Model = pricing.Price.Model
			detected.DisplayName = pricing.Price.DisplayName
			detected.InputUSDPerMTok = pricing.Price.InputUSDPerMTok
			detected.CachedInputUSDPerMTok = pricing.Price.CachedInputUSDPerMTok
			detected.OutputUSDPerMTok = pricing.Price.OutputUSDPerMTok
			detected.CacheWriteUSDPerMTok = pricing.Price.CacheWriteUSDPerMTok
		}
		snapshot.DetectedModels = append(snapshot.DetectedModels, detected)
	}
	sort.Slice(snapshot.DetectedModels, func(i, j int) bool {
		return snapshot.DetectedModels[i].ObservedModel < snapshot.DetectedModels[j].ObservedModel
	})
	return snapshot
}

func (m *PricingCatalogManager) ComputeDetailPricing(model string, tokens TokenStats) pricingTotals {
	canonical := normalizeCanonicalModelID(model)
	if canonical == "" {
		return pricingTotals{State: pricingStateUnpriced, Source: pricingSourceNone}
	}

	m.mu.RLock()
	entry, source, ok := m.lookupEffectiveModelLocked(canonical)
	m.mu.RUnlock()

	if !ok {
		switch builtinUnpricedModelState(canonical) {
		case pricingStateUnfinalized:
			return pricingTotals{
				State:           pricingStateUnfinalized,
				Unfinalized:     1,
				UniqueModelName: canonical,
				Source:          pricingSourceBuiltin,
			}
		default:
			return pricingTotals{
				State:           pricingStateUnpriced,
				Unpriced:        1,
				UniqueModelName: canonical,
				Source:          pricingSourceNone,
			}
		}
	}

	tokens = normaliseTokenStats(tokens)
	price := modelPrice{
		InputMicros:      usdPerMTokToMicros(entry.InputUSDPerMTok),
		OutputMicros:     usdPerMTokToMicros(entry.OutputUSDPerMTok),
		CacheReadMicros:  usdPerMTokToMicros(entry.CachedInputUSDPerMTok),
		CacheWriteMicros: usdPerMTokToMicros(entry.CacheWriteUSDPerMTok),
	}
	cacheRead := maxInt64(tokens.CacheReadTokens, tokens.CachedTokens)
	cacheWrite := tokens.CacheWriteTokens
	nonCachedInput := tokens.InputTokens
	if price.CacheWriteMicros == 0 {
		nonCachedInput -= cacheRead
		if nonCachedInput < 0 {
			nonCachedInput = 0
		}
	}

	costMicros := tokensToMicros(nonCachedInput, price.InputMicros)
	costMicros += tokensToMicros(tokens.OutputTokens, price.OutputMicros)
	costMicros += tokensToMicros(cacheRead, price.CacheReadMicros)
	costMicros += tokensToMicros(cacheWrite, price.CacheWriteMicros)
	return pricingTotals{
		CostMicros:      costMicros,
		State:           pricingStatePriced,
		UniqueModelName: canonical,
		Source:          source,
		Price:           entry,
	}
}

func (m *PricingCatalogManager) effectiveModelsLocked() map[string]PricingModel {
	result := builtinPricingModels()
	for key, model := range m.officialModels {
		result[key] = normalizePricingModel(model, key, pricingSourceOfficial)
	}
	for key, model := range m.overrides {
		result[key] = normalizePricingModel(model, key, pricingSourceOverride)
	}
	return result
}

func (m *PricingCatalogManager) lookupEffectiveModelLocked(model string) (PricingModel, string, bool) {
	if canonical, ok := m.aliasToCanonical[normalizeAliasKey(model)]; ok {
		model = canonical
	}
	if entry, ok := m.overrides[model]; ok {
		return normalizePricingModel(entry, model, pricingSourceOverride), pricingSourceOverride, true
	}
	if entry, ok := m.officialModels[model]; ok {
		return normalizePricingModel(entry, model, pricingSourceOfficial), pricingSourceOfficial, true
	}
	if entry, ok := builtinPricingModels()[model]; ok {
		return entry, pricingSourceBuiltin, true
	}
	return PricingModel{}, "", false
}

func (m *PricingCatalogManager) ensureSourceMetadataLocked() {
	for _, fetcher := range m.fetchers {
		source := m.sources[fetcher.ID]
		source.ID = fetcher.ID
		source.Label = fetcher.Label
		source.URL = fetcher.URL
		m.sources[fetcher.ID] = source
	}
}

func (m *PricingCatalogManager) rebuildAliasIndexLocked() {
	m.ensureSourceMetadataLocked()
	aliasToCanonical := make(map[string]string)
	register := func(canonical string, model PricingModel) {
		for _, alias := range aliasesForModel(canonical, model) {
			key := normalizeAliasKey(alias)
			if key == "" {
				continue
			}
			if _, exists := aliasToCanonical[key]; !exists {
				aliasToCanonical[key] = canonical
			}
		}
	}
	for canonical, model := range builtinPricingModels() {
		register(canonical, model)
	}
	for canonical, model := range m.officialModels {
		register(canonical, model)
	}
	for canonical, model := range m.overrides {
		register(canonical, model)
	}
	for _, canonical := range builtinUnfinalizedModels() {
		key := normalizeAliasKey(canonical)
		if _, exists := aliasToCanonical[key]; !exists {
			aliasToCanonical[key] = canonical
		}
	}
	m.aliasToCanonical = aliasToCanonical
}

func builtinPricingModels() map[string]PricingModel {
	models := map[string]PricingModel{
		"gpt-5.4":           pricingModelFromValues("gpt-5.4", "gpt-5.4", 2.5, 0.25, 15, 0, pricingSourceBuiltin),
		"gpt-5.2":           pricingModelFromValues("gpt-5.2", "gpt-5.2", 1.75, 0.175, 14, 0, pricingSourceBuiltin),
		"gpt-5.3-codex":     pricingModelFromValues("gpt-5.3-codex", "gpt-5.3-codex", 1.75, 0.175, 14, 0, pricingSourceBuiltin),
		"gpt-5.2-codex":     pricingModelFromValues("gpt-5.2-codex", "gpt-5.2-codex", 1.75, 0.175, 14, 0, pricingSourceBuiltin),
		"claude-opus-4-7":   pricingModelFromValues("claude-opus-4-7", "Claude Opus 4.7", 5, 0.5, 25, 6.25, pricingSourceBuiltin),
		"claude-opus-4-6":   pricingModelFromValues("claude-opus-4-6", "Claude Opus 4.6", 5, 0.5, 25, 6.25, pricingSourceBuiltin),
		"claude-sonnet-4-6": pricingModelFromValues("claude-sonnet-4-6", "Claude Sonnet 4.6", 3, 0.3, 15, 3.75, pricingSourceBuiltin),
		"claude-haiku-4-5":  pricingModelFromValues("claude-haiku-4-5", "Claude Haiku 4.5", 1, 0.1, 5, 1.25, pricingSourceBuiltin),
	}
	return models
}

func builtinUnfinalizedModels() []string {
	return []string{"gpt-5.3-codex-spark"}
}

func builtinUnpricedModelState(model string) pricingState {
	for _, canonical := range builtinUnfinalizedModels() {
		if canonical == model {
			return pricingStateUnfinalized
		}
	}
	return pricingStateUnpriced
}

func pricingModelFromValues(model, display string, input, cachedInput, output, cacheWrite float64, source string) PricingModel {
	return PricingModel{
		Model:                 model,
		DisplayName:           display,
		InputUSDPerMTok:       input,
		CachedInputUSDPerMTok: cachedInput,
		OutputUSDPerMTok:      output,
		CacheWriteUSDPerMTok:  cacheWrite,
		Source:                source,
		CanonicalModel:        model,
		PricingStatus:         pricingStatePriced,
	}
}

func fetchOpenAIPricing(ctx context.Context, client *http.Client, url string) (map[string]PricingModel, error) {
	body, err := fetchPricingBody(ctx, client, url)
	if err != nil {
		return nil, err
	}
	return parseOpenAIPricingHTML(body)
}

func fetchAnthropicPricing(ctx context.Context, client *http.Client, url string) (map[string]PricingModel, error) {
	body, err := fetchPricingBody(ctx, client, url)
	if err != nil {
		return nil, err
	}
	return parseAnthropicPricingHTML(body)
}

func fetchPricingBody(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", defaultPricingUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func parseOpenAIPricingHTML(body string) (map[string]PricingModel, error) {
	matches := openAIPricingRowPattern.FindAllStringSubmatch(html.UnescapeString(body), -1)
	if len(matches) == 0 {
		return nil, errors.New("no openai pricing rows found")
	}
	models := make(map[string]PricingModel)
	for _, match := range matches {
		display := cleanupOpenAIModelLabel(match[1])
		if display == "" {
			continue
		}
		lowerDisplay := strings.ToLower(display)
		if strings.Contains(lowerDisplay, "batch") {
			continue
		}
		canonical := normalizeCanonicalModelID(display)
		if canonical == "" {
			continue
		}
		if _, exists := models[canonical]; exists {
			continue
		}
		input, errInput := parsePriceFloat(match[2])
		cachedInput, errCached := parsePriceFloat(match[3])
		output, errOutput := parsePriceFloat(match[4])
		if errInput != nil || errCached != nil || errOutput != nil {
			continue
		}
		models[canonical] = pricingModelFromValues(canonical, display, input, cachedInput, output, 0, pricingSourceOfficial)
	}
	if len(models) == 0 {
		return nil, errors.New("no usable openai pricing rows found")
	}
	return models, nil
}

func parseAnthropicPricingHTML(body string) (map[string]PricingModel, error) {
	body = html.UnescapeString(body)
	parts := strings.Split(body, "card_pricing_title_text")
	if len(parts) <= 1 {
		return nil, errors.New("no anthropic pricing cards found")
	}
	models := make(map[string]PricingModel)
	for _, part := range parts[1:] {
		titleMatch := anthropicTitlePattern.FindStringSubmatch(part)
		if len(titleMatch) < 2 {
			continue
		}
		title := strings.TrimSpace(strings.ReplaceAll(titleMatch[1], "&nbsp;", " "))
		if title == "" {
			continue
		}
		values := anthropicValuePattern.FindAllStringSubmatch(part, 4)
		if len(values) < 4 {
			continue
		}
		input, errInput := parsePriceFloat(values[0][1])
		output, errOutput := parsePriceFloat(values[1][1])
		cacheWrite, errWrite := parsePriceFloat(values[2][1])
		cacheRead, errRead := parsePriceFloat(values[3][1])
		if errInput != nil || errOutput != nil || errWrite != nil || errRead != nil {
			continue
		}
		canonical := normalizeCanonicalModelID("claude-" + title)
		if canonical == "" {
			continue
		}
		models[canonical] = pricingModelFromValues(canonical, "Claude "+title, input, cacheRead, output, cacheWrite, pricingSourceOfficial)
	}
	if len(models) == 0 {
		return nil, errors.New("no usable anthropic pricing cards found")
	}
	return models, nil
}

func parsePriceFloat(raw string) (float64, error) {
	cleaned := strings.TrimSpace(raw)
	cleaned = strings.TrimPrefix(cleaned, "$")
	cleaned = strings.TrimSuffix(cleaned, "/MTok")
	cleaned = strings.ReplaceAll(cleaned, ",", "")
	return strconv.ParseFloat(cleaned, 64)
}

func cleanupOpenAIModelLabel(label string) string {
	label = strings.TrimSpace(parentheticalSuffix.ReplaceAllString(label, ""))
	label = strings.TrimSpace(strings.TrimSuffix(label, "-"))
	return label
}

func normalizeCanonicalModelID(model string) string {
	model = strings.TrimSpace(strings.ToLower(html.UnescapeString(model)))
	if model == "" {
		return ""
	}
	model = strings.TrimSpace(strings.Split(model, ",")[0])
	model = strings.TrimSpace(parentheticalSuffix.ReplaceAllString(model, ""))
	model = strings.Trim(model, "-_/ ")
	if idx := strings.LastIndex(model, "/"); idx >= 0 && idx < len(model)-1 {
		model = model[idx+1:]
	}

	switch {
	case strings.HasPrefix(model, "opus "):
		model = "claude-" + model
	case strings.HasPrefix(model, "sonnet "):
		model = "claude-" + model
	case strings.HasPrefix(model, "haiku "):
		model = "claude-" + model
	}

	model = strings.ReplaceAll(model, "_", "-")
	model = strings.ReplaceAll(model, " ", "-")
	model = strings.ReplaceAll(model, "claude-4.7-opus", "claude-opus-4.7")
	model = strings.ReplaceAll(model, "claude-4-7-opus", "claude-opus-4-7")
	model = strings.ReplaceAll(model, "claude-4.6-sonnet", "claude-sonnet-4.6")
	model = strings.ReplaceAll(model, "claude-4-6-sonnet", "claude-sonnet-4-6")
	model = strings.ReplaceAll(model, "claude-4.5-haiku", "claude-haiku-4.5")
	model = strings.ReplaceAll(model, "claude-4-5-haiku", "claude-haiku-4-5")

	normalized := normalizeAliasKey(model)
	normalized = trailingDatePattern.ReplaceAllString(normalized, "")
	normalized = trailingSnapshotPattern.ReplaceAllString(normalized, "")
	if strings.HasPrefix(normalized, "claude-opus-47") {
		normalized = strings.Replace(normalized, "claude-opus-47", "claude-opus-4-7", 1)
	}
	if strings.HasPrefix(normalized, "claude-sonnet-46") {
		normalized = strings.Replace(normalized, "claude-sonnet-46", "claude-sonnet-4-6", 1)
	}
	if strings.HasPrefix(normalized, "claude-haiku-45") {
		normalized = strings.Replace(normalized, "claude-haiku-45", "claude-haiku-4-5", 1)
	}
	if strings.HasPrefix(normalized, "gpt-54") {
		normalized = strings.Replace(normalized, "gpt-54", "gpt-5-4", 1)
	}
	if strings.HasPrefix(normalized, "gpt-52") {
		normalized = strings.Replace(normalized, "gpt-52", "gpt-5-2", 1)
	}
	if strings.HasPrefix(normalized, "gpt-53-codex") {
		normalized = strings.Replace(normalized, "gpt-53-codex", "gpt-5-3-codex", 1)
	}
	if strings.HasPrefix(normalized, "gpt-52-codex") {
		normalized = strings.Replace(normalized, "gpt-52-codex", "gpt-5-2-codex", 1)
	}
	if strings.HasPrefix(normalized, "gpt-53-codex-spark") {
		normalized = strings.Replace(normalized, "gpt-53-codex-spark", "gpt-5-3-codex-spark", 1)
	}
	normalized = strings.ReplaceAll(normalized, "4-7", "4-7")
	normalized = strings.ReplaceAll(normalized, "4-6", "4-6")
	normalized = strings.ReplaceAll(normalized, "4-5", "4-5")

	switch normalized {
	case "claude-opus-47":
		return "claude-opus-4-7"
	case "claude-sonnet-46":
		return "claude-sonnet-4-6"
	case "claude-haiku-45":
		return "claude-haiku-4-5"
	case "gpt-54":
		return "gpt-5.4"
	case "gpt-52":
		return "gpt-5.2"
	}

	if strings.HasPrefix(normalized, "gpt-5-4") {
		return "gpt-5.4"
	}
	if strings.HasPrefix(normalized, "gpt-5-2-codex") {
		return "gpt-5.2-codex"
	}
	if strings.HasPrefix(normalized, "gpt-5-3-codex-spark") {
		return "gpt-5.3-codex-spark"
	}
	if strings.HasPrefix(normalized, "gpt-5-3-codex") {
		return "gpt-5.3-codex"
	}
	if strings.HasPrefix(normalized, "gpt-5-2") {
		return "gpt-5.2"
	}
	if strings.HasPrefix(normalized, "claude-opus-4-7") {
		return "claude-opus-4-7"
	}
	if strings.HasPrefix(normalized, "claude-opus-4-6") {
		return "claude-opus-4-6"
	}
	if strings.HasPrefix(normalized, "claude-sonnet-4-6") {
		return "claude-sonnet-4-6"
	}
	if strings.HasPrefix(normalized, "claude-haiku-4-5") {
		return "claude-haiku-4-5"
	}
	return normalized
}

func normalizeAliasKey(model string) string {
	model = strings.TrimSpace(strings.ToLower(html.UnescapeString(model)))
	model = strings.ReplaceAll(model, ".", "-")
	model = strings.ReplaceAll(model, "_", "-")
	model = separatorPattern.ReplaceAllString(model, "-")
	model = strings.Trim(model, "-")
	return model
}

func aliasesForModel(canonical string, model PricingModel) []string {
	aliases := []string{
		canonical,
		strings.ReplaceAll(canonical, ".", "-"),
		strings.ReplaceAll(canonical, "-", "."),
		model.Model,
		model.DisplayName,
	}
	switch canonical {
	case "claude-opus-4-7":
		aliases = append(aliases, "claude-opus-4.7", "opus-4.7", "opus-4-7")
	case "claude-opus-4-6":
		aliases = append(aliases, "claude-opus-4.6", "opus-4.6", "opus-4-6")
	case "claude-sonnet-4-6":
		aliases = append(aliases, "claude-sonnet-4.6", "sonnet-4.6", "sonnet-4-6")
	case "claude-haiku-4-5":
		aliases = append(aliases, "claude-haiku-4.5", "haiku-4.5", "haiku-4-5")
	case "gpt-5.4":
		aliases = append(aliases, "gpt-5-4")
	case "gpt-5.2":
		aliases = append(aliases, "gpt-5-2")
	case "gpt-5.3-codex":
		aliases = append(aliases, "gpt-5-3-codex")
	case "gpt-5.2-codex":
		aliases = append(aliases, "gpt-5-2-codex")
	case "gpt-5.3-codex-spark":
		aliases = append(aliases, "gpt-5-3-codex-spark")
	}
	return aliases
}

func normalizePricingModel(entry PricingModel, canonical, source string) PricingModel {
	entry.CanonicalModel = canonical
	if entry.Model == "" {
		entry.Model = canonical
	}
	if entry.DisplayName == "" {
		entry.DisplayName = entry.Model
	}
	entry.Source = source
	return entry
}

func canonicalizePricingModelMap(input map[string]PricingModel, source string) map[string]PricingModel {
	result := make(map[string]PricingModel, len(input))
	for key, model := range input {
		canonical := normalizeCanonicalModelID(key)
		if canonical == "" {
			canonical = normalizeCanonicalModelID(model.CanonicalModel)
		}
		if canonical == "" {
			canonical = normalizeCanonicalModelID(model.Model)
		}
		if canonical == "" {
			continue
		}
		result[canonical] = normalizePricingModel(model, canonical, source)
	}
	return result
}

func clonePricingModelMap(input map[string]PricingModel) map[string]PricingModel {
	result := make(map[string]PricingModel, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func clonePricingSourceMap(input map[string]PricingSourceInfo) map[string]PricingSourceInfo {
	result := make(map[string]PricingSourceInfo, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func sortedPricingSourcesLocked(input map[string]PricingSourceInfo) []PricingSourceInfo {
	sources := make([]PricingSourceInfo, 0, len(input))
	for _, source := range input {
		sources = append(sources, source)
	}
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].ID < sources[j].ID
	})
	return sources
}

func usdPerMTokToMicros(value float64) int64 {
	if value <= 0 {
		return 0
	}
	return int64(math.Round(value * 1_000_000))
}

func tokensToMicros(tokens, rateMicrosPerMillion int64) int64 {
	if tokens <= 0 || rateMicrosPerMillion <= 0 {
		return 0
	}
	return int64(math.Round(float64(tokens) * float64(rateMicrosPerMillion) / 1_000_000.0))
}

func microsToUSD(micros int64) float64 {
	if micros == 0 {
		return 0
	}
	return float64(micros) / 1_000_000.0
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
