package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/net/http/httpguts"
)

func (h *Handler) TestOrdinaryAuthFileMessage(c *gin.Context) {
	h.testOrdinaryAuthFileMessage(c, apiCallPublicTransport)
}

func (h *Handler) testOrdinaryAuthFileMessage(c *gin.Context, transport http.RoundTripper) {
	var req struct {
		Name      string `json:"name"`
		AuthIndex string `json:"auth_index"`
		Model     string `json:"model"`
		Message   string `json:"message"`
		MaxTokens int    `json:"max_tokens"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		writeManualActionError(c, http.StatusBadRequest, "invalid test message request")
		return
	}
	req.Model, req.Message = strings.TrimSpace(req.Model), strings.TrimSpace(req.Message)
	if req.Model == "" || len(req.Model) > 256 || len(req.Message) > 4096 || req.MaxTokens < 0 || req.MaxTokens > 256 {
		writeManualActionError(c, http.StatusBadRequest, "choose a model and use a short message with at most 256 output tokens")
		return
	}
	if req.Message == "" {
		req.Message = "Reply with OK."
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = 32
	}
	target, status, message := h.ordinaryManualCredential(req.Name, req.AuthIndex)
	if status != 0 {
		writeManualActionError(c, status, message)
		return
	}
	cfg := h.configSnapshot()
	if cfg == nil || cfg.Home.Enabled || !apiCallDirect(h.apiCallProxy(target.auth)) {
		writeManualActionError(c, http.StatusNotImplemented, "direct public API testing is unavailable with this transport configuration")
		return
	}
	destination, header, key, err := manualMessageEndpoint(target.auth)
	if err != nil {
		writeManualActionError(c, http.StatusNotImplemented, err.Error())
		return
	}
	if !manualMessageHasModel(target.auth.ID, req.Model) {
		writeManualActionError(c, http.StatusBadRequest, "the model is not registered for the selected credential")
		return
	}
	current, exists := target.manager.GetByID(target.auth.ID)
	if !exists || !h.manualCredentialSameScope(target, current) || current.CredentialVersion != target.auth.CredentialVersion {
		writeManualActionError(c, http.StatusConflict, "credential changed; reload the credentials list")
		return
	}
	if !manualCredentialReady(c, current, req.Model) {
		return
	}
	model := target.manager.ResolveExecutionModel(current, req.Model)
	if model == "" {
		writeManualActionError(c, http.StatusBadRequest, "no public execution model is available for the selected model")
		return
	}
	tokenLimitField := "max_tokens"
	if strings.EqualFold(current.Provider, "openai") && destination.Hostname() == "api.openai.com" {
		tokenLimitField = "max_completion_tokens"
	}
	payload := map[string]any{
		"model": model, "messages": []map[string]string{{"role": "user", "content": req.Message}}, tokenLimitField: req.MaxTokens,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		writeManualActionError(c, http.StatusInternalServerError, "test message could not be encoded")
		return
	}
	upstream, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, destination.String(), bytes.NewReader(data))
	if err != nil {
		writeManualActionError(c, http.StatusBadRequest, "invalid upstream endpoint")
		return
	}
	upstream.Header = header
	upstream.Header.Set("Content-Type", "application/json")
	start := time.Now()
	response, err := boundedAPICallClient(transport, 25*time.Second).Do(upstream)
	if err != nil {
		status, message := manualUpstreamError(c, err, "the selected provider request failed")
		writeManualActionError(c, status, message)
		return
	}
	defer response.Body.Close()
	copyManualRetryAfter(c, response.Header)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := response.StatusCode
		if code == http.StatusTooManyRequests {
			target.manager.MarkResult(c.Request.Context(), coreauth.Result{
				AuthID: current.ID, Provider: current.Provider, Model: req.Model, RouteModel: req.Model,
				RegistrationEpoch: current.RegistrationEpoch, CredentialVersion: current.CredentialVersion,
				Error:      &coreauth.Error{Code: "rate_limited", Message: "provider requires a wait", HTTPStatus: code},
				RetryAfter: manualRetryDelay(response.Header.Get("Retry-After"), time.Now()), SkipQuotaObservation: true,
			})
		}
		if code < 400 || code > 599 {
			code = http.StatusBadGateway
		}
		c.JSON(code, gin.H{"status": "error", "supported": true, "upstream_status": response.StatusCode,
			"error": "the selected upstream API returned " + strconv.Itoa(response.StatusCode)})
		return
	}
	preview, err := manualMessagePreview(response.Body, header.Get("Anthropic-Version") != "")
	if err != nil {
		writeManualActionError(c, http.StatusBadGateway, "the selected provider returned no supported text response")
		return
	}
	preview = apiCallRedactor([]string{key}).Replace(preview)
	const maxPreviewCharacters = 512
	if chars := []rune(preview); len(chars) > maxPreviewCharacters {
		preview = string(chars[:maxPreviewCharacters])
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"status": "ok", "name": authFileListName(current), "auth_id": current.ID, "selected_auth_id": current.ID,
		"provider": current.Provider, "model": req.Model, "latency_ms": time.Since(start).Milliseconds(), "output_preview": preview,
	})
}

func manualMessageEndpoint(record *coreauth.Auth) (*url.URL, http.Header, string, error) {
	auth := record.Clone()
	key := strings.TrimSpace(auth.Attributes["api_key"])
	if key == "" {
		storedKey, _ := auth.Metadata["api_key"].(string)
		key = strings.TrimSpace(storedKey)
	}
	if key == "" || !httpguts.ValidHeaderFieldValue(key) {
		return nil, nil, "", errors.New("test messages require a stored public API key for the selected credential")
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["api_key"] = key
	base := strings.TrimSpace(auth.Attributes["base_url"])
	if base == "" {
		base, _ = auth.Metadata["base_url"].(string)
		base = strings.TrimSpace(base)
		if base != "" {
			auth.Attributes["base_url"] = base
		}
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	resource := "chat/completions"
	header := make(http.Header)
	switch provider {
	case "openai":
		if base == "" {
			base = "https://api.openai.com/v1"
		}
	case "xai":
		if base == "" {
			base = "https://api.x.ai/v1"
		}
	case "openai-compatibility":
		if base == "" {
			return nil, nil, "", errors.New("the selected connection has no configured HTTPS base URL")
		}
	case "claude", "anthropic":
		if base == "" {
			base = "https://api.anthropic.com"
		}
		resource = "v1/messages"
		header.Set("Anthropic-Version", "2023-06-01")
	default:
		return nil, nil, "", errors.New("the selected provider has no supported public test-message protocol")
	}
	target, err := apiCallURL(base)
	if err != nil || target.Scheme != "https" || target.RawQuery != "" || target.ForceQuery {
		return nil, nil, "", errors.New("the selected credential needs a public HTTPS API base without a query")
	}
	target = target.JoinPath(resource)
	token, err := ordinaryAPICallToken(auth, target)
	if err != nil {
		return nil, nil, "", err
	}
	if header.Get("Anthropic-Version") != "" {
		header.Set("X-Api-Key", token)
	} else {
		header.Set("Authorization", "Bearer "+token)
	}
	return target, header, token, nil
}

func manualMessageHasModel(id, wanted string) bool {
	for _, model := range registry.GetGlobalRegistry().GetModelsForClient(id) {
		if model != nil && model.ID == wanted {
			return true
		}
	}
	return false
}

func manualCredentialReady(c *gin.Context, auth *coreauth.Auth, model string) bool {
	now := time.Now()
	allowed, until := coreauth.SelectedCredentialAvailability(auth, model, now)
	if allowed {
		return true
	}
	if until.After(now) {
		c.Header("Retry-After", until.UTC().Format(http.TimeFormat))
		writeManualActionError(c, http.StatusTooManyRequests, "the selected credential is waiting for its current retry window")
	} else {
		writeManualActionError(c, http.StatusConflict, "the selected credential is currently unavailable")
	}
	return false
}

func manualMessagePreview(reader io.Reader, anthropic bool) (string, error) {
	const limit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || len(body) > limit {
		return "", errors.New("invalid response size")
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	if anthropic {
		for _, block := range result.Content {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				return strings.TrimSpace(block.Text), nil
			}
		}
	} else if len(result.Choices) != 0 && strings.TrimSpace(result.Choices[0].Message.Content) != "" {
		return strings.TrimSpace(result.Choices[0].Message.Content), nil
	}
	return "", errors.New("response text is missing")
}

func manualRetryDelay(raw string, now time.Time) *time.Duration {
	raw = strings.TrimSpace(raw)
	if s, err := strconv.ParseUint(raw, 10, 32); err == nil {
		delay := time.Duration(s) * time.Second
		return &delay
	}
	if date, err := http.ParseTime(raw); err == nil {
		delay := max(date.Sub(now), 0)
		return &delay
	}
	return nil
}
