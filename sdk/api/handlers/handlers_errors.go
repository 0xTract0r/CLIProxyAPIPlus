package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/net/context"
)

func statusFromError(err error) int {
	return clienterror.HTTPStatusFromError(err)
}

func isAuthSelectionUnavailable(err error) bool {
	type modelCooldownMarker interface {
		IsModelCooldown() bool
	}
	var mcm modelCooldownMarker
	if errors.As(err, &mcm) && mcm != nil && mcm.IsModelCooldown() {
		return false
	}

	var authErr *coreauth.Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	code := strings.TrimSpace(authErr.Code)
	return code == "auth_not_found" || code == "auth_unavailable"
}

func enrichAuthSelectionError(err error, providers []string, model string) error {
	if err == nil {
		return nil
	}

	type modelCooldownMarker interface {
		IsModelCooldown() bool
	}
	var mcm modelCooldownMarker
	if errors.As(err, &mcm) && mcm != nil && mcm.IsModelCooldown() {
		return err
	}

	var authErr *coreauth.Error
	if !errors.As(err, &authErr) || authErr == nil {
		return err
	}

	code := strings.TrimSpace(authErr.Code)
	if code != "auth_not_found" && code != "auth_unavailable" {
		return err
	}

	providerText := strings.Join(providers, ",")
	if providerText == "" {
		providerText = "unknown"
	}
	modelText := strings.TrimSpace(model)
	if modelText == "" {
		modelText = "unknown"
	}

	baseMessage := strings.TrimSpace(authErr.Message)
	if baseMessage == "" {
		baseMessage = "no auth available"
	}

	cause := errors.Unwrap(err)
	var upstreamSummary string
	if cause != nil {
		upstreamSummary = coreauth.ExtractUpstreamErrorSummary(cause.Error())
	}
	var detail string
	if upstreamSummary != "" && !strings.Contains(baseMessage, upstreamSummary) {
		detail = fmt.Sprintf("%s (providers=%s, model=%s; last upstream error: %s)", baseMessage, providerText, modelText, upstreamSummary)
	} else {
		detail = fmt.Sprintf("%s (providers=%s, model=%s)", baseMessage, providerText, modelText)
	}

	// Clarify the most common alias confusion between Anthropic route names and internal provider keys.
	if strings.Contains(","+providerText+",", ",claude,") {
		detail += "; check Claude auth/key session and cooldown state via /v0/management/auth-files"
	}

	status := authErr.HTTPStatus
	if status <= 0 {
		status = http.StatusServiceUnavailable
	}

	enriched := &coreauth.Error{
		Code:       authErr.Code,
		Message:    detail,
		Retryable:  authErr.Retryable,
		HTTPStatus: status,
	}
	var carrier interface{ WithAuthError(*coreauth.Error) error }
	if errors.As(err, &carrier) && carrier != nil {
		return carrier.WithAuthError(enriched)
	}
	if coreauth.IsTerminalAuthError(err) {
		return coreauth.NewTerminalAuthError(enriched, cause)
	}
	if cause != nil {
		return coreauth.WithCause(enriched, cause)
	}
	return enriched
}

// defaultAuthUnavailableRetryAfterSeconds is the conservative back-off hint used
// when the auth pool is fully exhausted. A short, fixed window keeps clients from
// hammering an empty pool while still letting them recover quickly once auth
// becomes available again. The auth-selection error carried here (a fresh
// *coreauth.Error built by enrichAuthSelectionError) never exposes a cooldown /
// RetryAfter hint of its own, so a fixed 30s is always used.
const defaultAuthUnavailableRetryAfterSeconds = 30

// authUnavailableRetryAfter reports the Retry-After hint (in seconds) that should
// accompany an auth_unavailable / auth_not_found style 503 response. This is a
// server-generated back-pressure hint, independent of upstream passthrough
// headers. It returns the hint and true only for auth-selection 503 errors;
// unrelated errors return "" and false so their responses stay untouched.
func authUnavailableRetryAfter(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var authErr *coreauth.Error
	if !errors.As(err, &authErr) || authErr == nil {
		return "", false
	}
	code := strings.TrimSpace(authErr.Code)
	if code != "auth_not_found" && code != "auth_unavailable" {
		return "", false
	}
	return strconv.Itoa(defaultAuthUnavailableRetryAfterSeconds), true
}

// WriteErrorResponse writes an error message to the response writer using the HTTP status embedded in the message.
func (h *BaseAPIHandler) WriteErrorResponse(c *gin.Context, msg *interfaces.ErrorMessage) {
	status := http.StatusInternalServerError
	if msg != nil && msg.StatusCode > 0 {
		status = msg.StatusCode
	}
	if msg != nil && msg.DirectResponse {
		writeDirectErrorResponse(c, status, msg)
		return
	}
	if msg != nil && msg.Error != nil {
		for _, value := range coreauth.SafeResponseHeaders(msg.Error).Values("Retry-After") {
			c.Writer.Header().Add("Retry-After", value)
		}
		// Anti-correlation back-pressure (fork): auth-selection 503s (auth_not_found /
		// auth_unavailable) carry no Retry-After of their own, so synthesize a short,
		// fixed hint directly from the error code. This is independent of the passthrough
		// gate and of msg.Addon, so it never relaxes the Addon passthrough below.
		if retryAfter, ok := authUnavailableRetryAfter(msg.Error); ok {
			c.Writer.Header().Set("Retry-After", retryAfter)
		}
	}
	if msg != nil && msg.Addon != nil && PassthroughHeadersEnabled(h.CurrentConfig()) {
		for key, values := range msg.Addon {
			if len(values) == 0 || IsCPAReservedResponseHeader(key) {
				continue
			}
			c.Writer.Header().Del(key)
			for _, value := range values {
				c.Writer.Header().Add(key, value)
			}
		}
	}

	errText := http.StatusText(status)
	if msg != nil && msg.Error != nil {
		if v := strings.TrimSpace(msg.Error.Error()); v != "" {
			errText = v
		}
	}

	var errCause error
	if msg != nil {
		errCause = msg.Error
	}
	body := BuildErrorResponseBodyWithError(status, errText, errCause)
	// Append first to preserve upstream response logs, then drop duplicate payloads if already recorded.
	var previous []byte
	if existing, exists := c.Get("API_RESPONSE"); exists {
		if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
			previous = existingBytes
		}
	}
	appendAPIResponse(c, body)
	trimmedErrText := strings.TrimSpace(errText)
	trimmedBody := bytes.TrimSpace(body)
	if len(previous) > 0 {
		if (trimmedErrText != "" && bytes.Contains(previous, []byte(trimmedErrText))) ||
			(len(trimmedBody) > 0 && bytes.Contains(previous, trimmedBody)) {
			c.Set("API_RESPONSE", previous)
		}
	}

	if !c.Writer.Written() {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Status(status)
	_, _ = c.Writer.Write(body)
}

func writeDirectErrorResponse(c *gin.Context, status int, msg *interfaces.ErrorMessage) {
	for key, values := range FilterUpstreamHeaders(msg.Headers) {
		if len(values) == 0 || IsCPAReservedResponseHeader(key) {
			continue
		}
		c.Writer.Header().Del(key)
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}
	body := bytes.Clone(msg.Body)
	appendAPIResponse(c, body)
	if !c.Writer.Written() && c.Writer.Header().Get("Content-Type") == "" {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Status(status)
	_, _ = c.Writer.Write(body)
}

func (h *BaseAPIHandler) LoggingAPIResponseError(ctx context.Context, err *interfaces.ErrorMessage) {
	if cfg := h.CurrentConfig(); cfg != nil && cfg.RequestLog {
		if ginContext, ok := ctx.Value("gin").(*gin.Context); ok {
			if apiResponseErrors, isExist := ginContext.Get("API_RESPONSE_ERROR"); isExist {
				if slicesAPIResponseError, isOk := apiResponseErrors.([]*interfaces.ErrorMessage); isOk {
					slicesAPIResponseError = append(slicesAPIResponseError, err)
					ginContext.Set("API_RESPONSE_ERROR", slicesAPIResponseError)
				}
			} else {
				// Create new response data entry
				ginContext.Set("API_RESPONSE_ERROR", []*interfaces.ErrorMessage{err})
			}
		}
	}
}
