package registry

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// ModelsRefreshInterval is the shared cadence for model catalogs and account entitlements.
const ModelsRefreshInterval = 3 * time.Hour
const modelsFetchTimeout = 30 * time.Second

var modelsURLs = []string{
	"https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/models.json",
	"https://models.router-for.me/models.json",
}

//go:embed models/models.json
var embeddedModelsJSON []byte

type modelStore struct {
	mu   sync.RWMutex
	data *staticModelsJSON
}

var modelsCatalogStore = &modelStore{}

var modelsFetchConfig = struct {
	mu       sync.RWMutex
	proxyURL string
}{}

// ModelRefreshCallback is invoked when startup or periodic model refresh detects changes.
// changedProviders contains the provider names whose model definitions changed.
type ModelRefreshCallback func(changedProviders []string)

var (
	refreshCallbackMu     sync.Mutex
	refreshCallback       ModelRefreshCallback
	pendingRefreshChanges []string
)

// SetModelRefreshCallback registers a callback that is invoked when startup or
// periodic model refresh detects changes. Only one callback is supported;
// subsequent calls replace the previous callback.
func SetModelRefreshCallback(cb ModelRefreshCallback) {
	refreshCallbackMu.Lock()
	refreshCallback = cb
	var pending []string
	if cb != nil && len(pendingRefreshChanges) > 0 {
		pending = append([]string(nil), pendingRefreshChanges...)
		pendingRefreshChanges = nil
	}
	refreshCallbackMu.Unlock()

	if cb != nil && len(pending) > 0 {
		cb(pending)
	}
}

func init() {
	// Load embedded data as fallback on startup.
	if err := loadModelsFromBytes(embeddedModelsJSON, "embed"); err != nil {
		log.Warnf("registry: failed to parse embedded models.json (embedded catalog may be incomplete or invalid; continuing startup and will rely on remote model refresh): %v", err)
	}
}

// SetModelsFetchProxy configures the proxy used by remote models catalog refreshes.
// An empty value inherits environment proxy behavior; "direct"/"none" disables proxies.
func SetModelsFetchProxy(proxyURL string) {
	modelsFetchConfig.mu.Lock()
	modelsFetchConfig.proxyURL = strings.TrimSpace(proxyURL)
	modelsFetchConfig.mu.Unlock()
}

// StartModelsUpdater starts a background updater that fetches models
// immediately on startup and then refreshes the model catalog every 3 hours.
// Safe to call multiple times; only one updater will run.
func StartModelsUpdater(ctx context.Context) {
	generalCatalogUpdater.configure(ctx, "")
}

func newModelsFetchClient() *http.Client {
	client := &http.Client{Timeout: modelsFetchTimeout}

	modelsFetchConfig.mu.RLock()
	proxyURL := modelsFetchConfig.proxyURL
	modelsFetchConfig.mu.RUnlock()

	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Warnf("models updater: failed to configure proxy %q: %v", proxyURL, errBuild)
		return client
	}
	if transport != nil {
		client.Transport = transport
	}
	return client
}

func mergeMissingModelSections(data, fallback *staticModelsJSON) *staticModelsJSON {
	if data == nil {
		return fallback
	}
	if fallback == nil {
		return data
	}

	merged := *data
	if len(merged.Claude) == 0 {
		merged.Claude = cloneModelInfos(fallback.Claude)
	}
	if len(merged.Gemini) == 0 {
		merged.Gemini = cloneModelInfos(fallback.Gemini)
	}
	if len(merged.Vertex) == 0 {
		merged.Vertex = cloneModelInfos(fallback.Vertex)
	}
	if len(merged.GeminiCLI) == 0 {
		merged.GeminiCLI = cloneModelInfos(fallback.GeminiCLI)
	}
	if len(merged.AIStudio) == 0 {
		merged.AIStudio = cloneModelInfos(fallback.AIStudio)
	}
	if len(merged.CodexFree) == 0 {
		merged.CodexFree = cloneModelInfos(fallback.CodexFree)
	}
	if len(merged.CodexTeam) == 0 {
		merged.CodexTeam = cloneModelInfos(fallback.CodexTeam)
	}
	if len(merged.CodexPlus) == 0 {
		merged.CodexPlus = cloneModelInfos(fallback.CodexPlus)
	}
	if len(merged.CodexPro) == 0 {
		merged.CodexPro = cloneModelInfos(fallback.CodexPro)
	}
	if len(merged.Kimi) == 0 {
		merged.Kimi = cloneModelInfos(fallback.Kimi)
	}
	if len(merged.Antigravity) == 0 {
		merged.Antigravity = cloneModelInfos(fallback.Antigravity)
	}
	if len(merged.XAI) == 0 {
		merged.XAI = cloneModelInfos(fallback.XAI)
	}
	return &merged
}

// detectChangedProviders compares two model catalogs and returns provider names
// whose model definitions differ. Gemini changes affect both Gemini protocols,
// while Codex tiers (free/team/plus/pro) are grouped under one "codex" provider.
func detectChangedProviders(oldData, newData *staticModelsJSON) []string {
	if oldData == nil || newData == nil {
		return nil
	}

	type section struct {
		provider string
		oldList  []*ModelInfo
		newList  []*ModelInfo
	}

	sections := []section{
		{"claude", oldData.Claude, newData.Claude},
		{"gemini", oldData.Gemini, newData.Gemini},
		{"gemini-interactions", oldData.Gemini, newData.Gemini},
		{"vertex", oldData.Vertex, newData.Vertex},
		{"aistudio", oldData.AIStudio, newData.AIStudio},
		{"codex", oldData.CodexFree, newData.CodexFree},
		{"codex", oldData.CodexTeam, newData.CodexTeam},
		{"codex", oldData.CodexPlus, newData.CodexPlus},
		{"codex", oldData.CodexPro, newData.CodexPro},
		{"kimi", oldData.Kimi, newData.Kimi},
		{"kimi-ai", oldData.Kimi, newData.Kimi},
		{"kimi.ai", oldData.Kimi, newData.Kimi},
		{"kimi.com", oldData.Kimi, newData.Kimi},
		{"antigravity", oldData.Antigravity, newData.Antigravity},
		{"xai", oldData.XAI, newData.XAI},
		{"devin", oldData.Devin, newData.Devin},
		{"meta", oldData.Meta, newData.Meta},
	}

	seen := make(map[string]bool, len(sections))
	var changed []string
	for _, s := range sections {
		if seen[s.provider] {
			continue
		}
		if modelSectionChanged(s.oldList, s.newList) {
			changed = append(changed, s.provider)
			seen[s.provider] = true
		}
	}
	return changed
}

// modelSectionChanged reports whether two model slices differ, including
// internal metadata that is intentionally omitted from normal JSON responses.
func modelSectionChanged(a, b []*ModelInfo) bool {
	if len(a) != len(b) {
		return true
	}
	if len(a) == 0 {
		return false
	}
	for i := range a {
		if a[i] != nil && b[i] != nil {
			if !reflect.DeepEqual(a[i].NativeCapabilities, b[i].NativeCapabilities) || a[i].SupportConfigurationUpdate != b[i].SupportConfigurationUpdate {
				return true
			}
		}
	}
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return true
	}
	return string(aj) != string(bj)
}

func notifyModelRefresh(changedProviders []string) {
	if len(changedProviders) == 0 {
		return
	}

	refreshCallbackMu.Lock()
	cb := refreshCallback
	if cb == nil {
		pendingRefreshChanges = mergeProviderNames(pendingRefreshChanges, changedProviders)
		refreshCallbackMu.Unlock()
		return
	}
	refreshCallbackMu.Unlock()
	cb(changedProviders)
}

func mergeProviderNames(existing, incoming []string) []string {
	if len(incoming) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	merged := make([]string, 0, len(existing)+len(incoming))
	for _, provider := range existing {
		name := strings.ToLower(strings.TrimSpace(provider))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		merged = append(merged, name)
	}
	for _, provider := range incoming {
		name := strings.ToLower(strings.TrimSpace(provider))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		merged = append(merged, name)
	}
	return merged
}

func loadModelsFromBytes(data []byte, source string) error {
	var parsed staticModelsJSON
	if err := json.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("%s: decode models catalog: %w", source, err)
	}
	applyCodexCatalogCompatibility(&parsed)
	if err := validateModelsCatalog(&parsed); err != nil {
		return fmt.Errorf("%s: validate models catalog: %w", source, err)
	}

	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &parsed
	modelsCatalogStore.mu.Unlock()
	return nil
}

func getModels() *staticModelsJSON {
	modelsCatalogStore.mu.RLock()
	defer modelsCatalogStore.mu.RUnlock()
	return modelsCatalogStore.data
}

func validateModelsCatalog(data *staticModelsJSON) error {
	if data == nil {
		return fmt.Errorf("catalog is nil")
	}

	requiredSections := []struct {
		name   string
		models []*ModelInfo
	}{
		{name: "claude", models: data.Claude},
		{name: "gemini", models: data.Gemini},
		{name: "vertex", models: data.Vertex},
		{name: "aistudio", models: data.AIStudio},
		{name: "codex-free", models: data.CodexFree},
		{name: "codex-team", models: data.CodexTeam},
		{name: "codex-plus", models: data.CodexPlus},
		{name: "codex-pro", models: data.CodexPro},
		{name: "kimi", models: data.Kimi},
		{name: "antigravity", models: data.Antigravity},
		{name: "xai", models: data.XAI},
		{name: "meta", models: data.Meta},
	}

	for _, section := range requiredSections {
		if err := validateModelSection(section.name, section.models); err != nil {
			return err
		}
	}
	return nil
}

func validateModelSection(section string, models []*ModelInfo) error {
	if len(models) == 0 {
		log.Warnf("models catalog: %s section is empty, continuing without those model definitions", section)
		return nil
	}

	seen := make(map[string]struct{}, len(models))
	for i, model := range models {
		if model == nil {
			return fmt.Errorf("%s[%d] is null", section, i)
		}
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			return fmt.Errorf("%s[%d] has empty id", section, i)
		}
		if _, exists := seen[modelID]; exists {
			return fmt.Errorf("%s contains duplicate model id %q", section, modelID)
		}
		seen[modelID] = struct{}{}
	}
	return nil
}
