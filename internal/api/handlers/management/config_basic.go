package management

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const (
	latestReleaseURL       = "https://api.github.com/repos/router-for-me/CLIProxyAPI/releases/latest"
	latestReleaseUserAgent = "CLIProxyAPI"
)

func (h *Handler) GetConfig(c *gin.Context) {
	snapshot := h.configSnapshot()
	if snapshot == nil {
		c.JSON(http.StatusOK, gin.H{})
		return
	}
	c.JSON(http.StatusOK, snapshot)
}

type releaseInfo struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
}

func setLatestReleaseRequestHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", latestReleaseUserAgent)
	if token := util.ResolveGitHubToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// GetLatestVersion returns the latest release version from GitHub without downloading assets.
func (h *Handler) GetLatestVersion(c *gin.Context) {
	client := &http.Client{Timeout: 10 * time.Second}
	proxyURL := ""
	if h != nil && h.cfg != nil {
		proxyURL = strings.TrimSpace(h.cfg.ProxyURL)
	}
	if proxyURL != "" {
		sdkCfg := &sdkconfig.SDKConfig{ProxyURL: proxyURL}
		util.SetProxy(sdkCfg, client)
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "request_create_failed", "message": err.Error()})
		return
	}
	setLatestReleaseRequestHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "request_failed", "message": err.Error()})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close latest version response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.JSON(http.StatusBadGateway, gin.H{"error": "unexpected_status", "message": fmt.Sprintf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))})
		return
	}

	var info releaseInfo
	if errDecode := json.NewDecoder(resp.Body).Decode(&info); errDecode != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "decode_failed", "message": errDecode.Error()})
		return
	}

	version := strings.TrimSpace(info.TagName)
	if version == "" {
		version = strings.TrimSpace(info.Name)
	}
	if version == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid_response", "message": "missing release version"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"latest-version": version})
}

func WriteConfig(path string, data []byte) error {
	doc, err := config.DecodeFirstConfigDocument(data)
	if err != nil {
		return err
	}
	var commented []config.ArchivedV8Section
	if config.IsV8ConfigLayout(doc.Content[0]) {
		normalized, _, sections, errNorm := config.NormalizeConfigLayoutSilent(data, true)
		if errNorm != nil {
			return errNorm
		}
		if errValidate := config.ValidateConfigSave(data, normalized); errValidate != nil {
			return errValidate
		}
		data, commented = normalized, sections
	}
	onDisk, err := config.ReadConfigFileSnapshot(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		err = config.CreateConfigFile(path, data)
	case err == nil:
		_, err = config.WriteConfigFileCAS(path, onDisk, data)
	}
	if err != nil {
		return err
	}
	config.WarnUnrecognizedV8Sections(commented)
	return nil
}

func (h *Handler) PutConfigYAML(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": "cannot read request body"})
		return
	}
	var cfg config.Config
	if err = yaml.Unmarshal(body, &cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": err.Error()})
		return
	}
	// Validate config using LoadConfigOptional with optional=false to enforce parsing
	tmpDir := filepath.Dir(h.configFilePath)
	tmpFile, err := os.CreateTemp(tmpDir, "config-validate-*.yaml")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": err.Error()})
		return
	}
	tempFile := tmpFile.Name()
	if _, errWrite := tmpFile.Write(body); errWrite != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tempFile)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": errWrite.Error()})
		return
	}
	if errClose := tmpFile.Close(); errClose != nil {
		_ = os.Remove(tempFile)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": errClose.Error()})
		return
	}
	defer func() {
		_ = os.Remove(tempFile)
	}()
	_, err = config.LoadConfigOptional(tempFile, false)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": err.Error()})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if WriteConfig(h.configFilePath, body) != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": "failed to write config"})
		return
	}
	// Reload into handler to keep memory in sync
	newCfg, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "reload_failed", "message": err.Error()})
		return
	}
	h.publishFileOAuthConfigLocked(newCfg)
	c.JSON(http.StatusOK, gin.H{"ok": true, "changed": []string{"config"}})
}

// GetConfigYAML returns the raw config.yaml file bytes without re-encoding.
// It preserves comments and original formatting/styles.
func (h *Handler) GetConfigYAML(c *gin.Context) {
	data, err := config.ReadConfigFile(h.configFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found", "message": "config file not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed", "message": err.Error()})
		return
	}
	c.Header("Content-Type", "application/yaml; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	// Write raw bytes as-is
	_, _ = c.Writer.Write(data)
}

// Debug
func (h *Handler) GetDebug(c *gin.Context) { c.JSON(200, gin.H{"debug": h.cfg.Debug}) }
func (h *Handler) PutDebug(c *gin.Context) { h.updateBoolField(c, func(v bool) { h.cfg.Debug = v }) }

// UsageStatisticsEnabled
func (h *Handler) GetUsageStatisticsEnabled(c *gin.Context) {
	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	v := h.cfg.UsageStatisticsEnabled
	h.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{"usage-statistics-enabled": v})
}
func (h *Handler) PutUsageStatisticsEnabled(c *gin.Context) {
	var b struct {
		Value *bool `json:"value"`
	}
	if err := c.ShouldBindJSON(&b); err != nil || b.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	v := *b.Value

	saveBasicField(h, c, func(cfg *config.Config) *bool { return &cfg.UsageStatisticsEnabled }, v)
}

// LoggingToFile
func (h *Handler) GetLoggingToFile(c *gin.Context) {
	readLogConfigField(h, c, "logging-to-file", func(cfg *config.Config) bool { return cfg.LoggingToFile })
}
func (h *Handler) PutLoggingToFile(c *gin.Context) {
	putLogBoolField(h, c, func(cfg *config.Config) *bool { return &cfg.LoggingToFile })
}

// LogsMaxTotalSizeMB
func (h *Handler) GetLogsMaxTotalSizeMB(c *gin.Context) {
	readLogConfigField(h, c, "logs-max-total-size-mb", func(cfg *config.Config) int { return cfg.LogsMaxTotalSizeMB })
}
func (h *Handler) PutLogsMaxTotalSizeMB(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	value := *body.Value
	if value < 0 {
		value = 0
	}
	saveLogConfigField(h, c, func(cfg *config.Config) *int { return &cfg.LogsMaxTotalSizeMB }, value)
}

// ErrorLogsMaxFiles
func (h *Handler) GetErrorLogsMaxFiles(c *gin.Context) {
	readLogConfigField(h, c, "error-logs-max-files", func(cfg *config.Config) int { return cfg.ErrorLogsMaxFiles })
}
func (h *Handler) PutErrorLogsMaxFiles(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	value := *body.Value
	if value < 0 {
		value = 10
	}
	saveLogConfigField(h, c, func(cfg *config.Config) *int { return &cfg.ErrorLogsMaxFiles }, value)
}

// Request log
func (h *Handler) GetRequestLog(c *gin.Context) {
	readLogConfigField(h, c, "request-log", func(cfg *config.Config) bool { return cfg.RequestLog })
}
func (h *Handler) PutRequestLog(c *gin.Context) {
	putLogBoolField(h, c, func(cfg *config.Config) *bool { return &cfg.RequestLog })
}

func readLogConfigField[T any](h *Handler, c *gin.Context, key string, field func(*config.Config) T) {
	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	value := field(h.cfg)
	h.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{key: value})
}

func putLogBoolField(h *Handler, c *gin.Context, field func(*config.Config) *bool) {
	var body struct {
		Value *bool `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	saveLogConfigField(h, c, field, *body.Value)
}

func saveLogConfigField[T any](h *Handler, c *gin.Context, field func(*config.Config) *T, value T) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	draft := h.cfg.CloneForRuntime()
	*field(draft) = value
	if err := commitConfigFieldLocked(h, c, draft, field(h.cfg), value); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", err)})
		return
	}
	h.finishConfigSaveLocked(c)
}

// Websocket auth
func (h *Handler) GetWebsocketAuth(c *gin.Context) {
	c.JSON(200, gin.H{"ws-auth": h.cfg.WebsocketAuth})
}
func (h *Handler) PutWebsocketAuth(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.WebsocketAuth = v })
}

// Request retry
func (h *Handler) GetRequestRetry(c *gin.Context) {
	c.JSON(200, gin.H{"request-retry": h.cfg.RequestRetry})
}
func (h *Handler) PutRequestRetry(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.RequestRetry = v })
}

// Max retry credentials
func (h *Handler) GetMaxRetryCredentials(c *gin.Context) {
	c.JSON(200, gin.H{"max-retry-credentials": h.cfg.MaxRetryCredentials})
}
func (h *Handler) PutMaxRetryCredentials(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.MaxRetryCredentials = v })
}

// Max retry interval
func (h *Handler) GetMaxRetryInterval(c *gin.Context) {
	c.JSON(200, gin.H{"max-retry-interval": h.cfg.MaxRetryInterval})
}
func (h *Handler) PutMaxRetryInterval(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.MaxRetryInterval = v })
}

// ForceModelPrefix
func (h *Handler) GetForceModelPrefix(c *gin.Context) {
	c.JSON(200, gin.H{"force-model-prefix": h.cfg.ForceModelPrefix})
}
func (h *Handler) PutForceModelPrefix(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.ForceModelPrefix = v })
}

func normalizeRoutingStrategy(strategy string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(strategy))
	switch normalized {
	case "", "round-robin", "roundrobin", "rr":
		return "round-robin", true
	case "weighted-round-robin", "weightedroundrobin", "wrr":
		return "weighted-round-robin", true
	case "fill-first", "fillfirst", "ff":
		return "fill-first", true
	default:
		return "", false
	}
}

// RoutingStrategy
func (h *Handler) GetRoutingStrategy(c *gin.Context) {
	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	current := h.cfg.Routing.Strategy
	h.mu.Unlock()
	strategy, ok := normalizeRoutingStrategy(current)
	if !ok {
		c.JSON(200, gin.H{"strategy": strings.TrimSpace(current)})
		return
	}
	c.JSON(200, gin.H{"strategy": strategy})
}
func (h *Handler) PutRoutingStrategy(c *gin.Context) {
	var body struct {
		Value *string `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	normalized, ok := normalizeRoutingStrategy(*body.Value)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid strategy"})
		return
	}
	saveBasicField(h, c, func(cfg *config.Config) *string { return &cfg.Routing.Strategy }, normalized)
}

// Proxy URL
func (h *Handler) GetProxyURL(c *gin.Context) {
	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	v := h.cfg.ProxyURL
	h.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{"proxy-url": v})
}
func (h *Handler) PutProxyURL(c *gin.Context) {
	var b struct {
		Value *string `json:"value"`
	}
	if err := c.ShouldBindJSON(&b); err != nil || b.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	v := *b.Value
	v = strings.TrimSpace(v)
	setting, err := proxyutil.Parse(v)
	if err != nil || (setting.Mode == proxyutil.ModeProxy && !proxyutil.ValidRequestProxy(v)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid proxy URL"})
		return
	}
	saveBasicField(h, c, func(cfg *config.Config) *string { return &cfg.ProxyURL }, v)
}
func (h *Handler) DeleteProxyURL(c *gin.Context) {
	saveBasicField(h, c, func(cfg *config.Config) *string { return &cfg.ProxyURL }, "")
}

// saveBasicField commits one scalar to disk before publishing the in-memory value.
// Holding the handler lock across both steps also preserves concurrent sibling edits.
func saveBasicField[T any](h *Handler, c *gin.Context, field func(*config.Config) *T, v T) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	draft := h.cfg.CloneForRuntime()
	*field(draft) = v
	persistProviderFieldLocked(h, c, draft, field(h.cfg), v)
}

func (h *Handler) GetLogsCompressAfterDays(c *gin.Context) {
	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	v := h.cfg.LogsCompressAfterDays
	h.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{"logs-compress-after-days": v})
}
func (h *Handler) PutLogsCompressAfterDays(c *gin.Context) {
	var b struct {
		Value *int `json:"value"`
	}
	if err := c.ShouldBindJSON(&b); err != nil || b.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	v := *b.Value
	if v < 0 {
		v = config.DefaultLogsCompressAfterDays
	}
	saveBasicField(h, c, func(cfg *config.Config) *int { return &cfg.LogsCompressAfterDays }, v)
}

func (h *Handler) GetLogsDeleteAfterDays(c *gin.Context) {
	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	v := h.cfg.LogsDeleteAfterDays
	h.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{"logs-delete-after-days": v})
}
func (h *Handler) PutLogsDeleteAfterDays(c *gin.Context) {
	var b struct {
		Value *int `json:"value"`
	}
	if err := c.ShouldBindJSON(&b); err != nil || b.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	v := *b.Value
	if v < 0 {
		v = config.DefaultLogsDeleteAfterDays
	}
	saveBasicField(h, c, func(cfg *config.Config) *int { return &cfg.LogsDeleteAfterDays }, v)
}
