// Farm account pinning ("串号止血") lives here: a gated inbound header that
// locks a single request onto exactly one upstream account and fail-closes
// (never falls back to another account) when that account is unavailable.
//
// This file only wires the TRIGGER and GATING. The fail-closed guarantee itself
// is provided by the pre-existing pinned_auth_id selection primitive in
// sdk/cliproxy/auth (RoundRobin/SessionAffinity selectors and the scheduler all
// restrict selection to the pinned auth and return an auth-unavailable error
// instead of rotating to a different account). Nothing here changes selection
// for non-farm traffic: it is a strict no-op unless FARM_PIN_ENABLED is set.
//
// Future work (A2): the same primitive can be triggered by a per-container
// account-scoped key -> auth mapping instead of a request header, without
// touching the fail-closed core.
package handlers

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// FarmAccountPinHeader is the inbound header a gated farm caller sets to force
// selection onto exactly one upstream account.
const FarmAccountPinHeader = "X-Farm-Account-Pin"

// farmPinMarkerMetadataKey records the raw farm pin value honoured for the
// current request. It is consumed only by error annotation so a fail-closed pin
// surfaces a distinct, observable auth_pinned_unavailable code instead of a
// generic pool-exhaustion error. It never influences selection.
const farmPinMarkerMetadataKey = "farm_account_pin"

// farmPinAuthUnavailableCode is the distinct error code surfaced when a pinned
// farm account is unavailable and the request was fail-closed (no fallback).
const farmPinAuthUnavailableCode = "auth_pinned_unavailable"

// farmPinConfig is the request-time gating snapshot read from the environment.
type farmPinConfig struct {
	enabled     bool
	allowedKeys map[string]struct{}
}

// farmPinConfigFromEnv reads the farm-pin gating configuration from the
// environment. The config is intentionally env-driven (like MANAGEMENT_PASSWORD
// and the GITLAB_* toggles) so enabling the primitive requires no config schema
// change and stays fully decoupled from non-farm request handling.
func farmPinConfigFromEnv() farmPinConfig {
	cfg := farmPinConfig{enabled: farmPinEnvEnabled()}
	raw := strings.TrimSpace(os.Getenv("FARM_PIN_ALLOWED_KEYS"))
	if raw == "" {
		return cfg
	}
	allowed := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		key := strings.TrimSpace(part)
		if key != "" {
			allowed[key] = struct{}{}
		}
	}
	cfg.allowedKeys = allowed
	return cfg
}

func farmPinEnvEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FARM_PIN_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// principalAllowed reports whether the authenticated caller may use the farm pin
// header. An empty allow-list means the FARM_PIN_ENABLED master switch alone
// authorises any authenticated caller; a non-empty allow-list narrows it to the
// listed farm client keys only, so an external caller that merely learns the
// header name cannot pin someone else's account.
func (c farmPinConfig) principalAllowed(principal string) bool {
	if len(c.allowedKeys) == 0 {
		return true
	}
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return false
	}
	_, ok := c.allowedKeys[principal]
	return ok
}

// applyFarmAccountPin resolves an optional, gated farm account pin from the
// inbound request and writes it into reqMeta as pinned_auth_id, so the existing
// pinned-auth selection primitive locks execution to exactly one account and
// fail-closes when that account is unavailable.
//
// It is a strict no-op unless FARM_PIN_ENABLED is set, which keeps non-farm
// traffic byte-identical to today's selection behaviour.
func (h *BaseAPIHandler) applyFarmAccountPin(ctx context.Context, reqMeta map[string]any) {
	if h == nil || reqMeta == nil {
		return
	}
	cfg := farmPinConfigFromEnv()
	if !cfg.enabled {
		return
	}
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return
	}
	pinValue := strings.TrimSpace(ginCtx.GetHeader(FarmAccountPinHeader))
	if pinValue == "" {
		return
	}
	// Never override an existing pin (e.g. websocket continuation) that was set
	// upstream of this request-metadata assembly.
	if existing, okExisting := reqMeta[coreexecutor.PinnedAuthMetadataKey].(string); okExisting && strings.TrimSpace(existing) != "" {
		return
	}
	principal := farmPinPrincipal(ginCtx)
	if !cfg.principalAllowed(principal) {
		log.WithField("request_id", logging.GetRequestID(ctx)).
			Warnf("farm-pin: ignoring %s header from unauthorised caller (not in FARM_PIN_ALLOWED_KEYS)", FarmAccountPinHeader)
		return
	}

	// Resolve a friendly account name to its auth ID when possible. On an
	// unknown/ambiguous name we still pin the raw value: no auth will match it,
	// so selection fail-closes with an auth-unavailable error rather than
	// silently serving a different account.
	pinnedID := pinValue
	if h.AuthManager != nil {
		if resolved, resolvedOK := h.AuthManager.ResolveFarmPinAuthID(pinValue); resolvedOK {
			pinnedID = resolved
		}
	}
	reqMeta[coreexecutor.PinnedAuthMetadataKey] = pinnedID
	reqMeta[farmPinMarkerMetadataKey] = pinValue
	log.WithField("request_id", logging.GetRequestID(ctx)).
		Infof("farm-pin: request locked to a single account (fail-closed, no fallback)")
}

// farmPinPrincipal returns the authenticated principal (client key) recorded by
// the auth middleware for the current request, used for allow-list gating.
func farmPinPrincipal(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get("userApiKey"); ok {
		if s, okStr := v.(string); okStr {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// annotateFarmPinError relabels a fail-closed farm-pin selection failure with a
// distinct, observable code so operators can tell "the pinned account is
// unavailable" apart from generic pool exhaustion. It only rewrites the code and
// message; it never changes the HTTP status or control flow, and it is a no-op
// for non-farm requests (marker absent).
func annotateFarmPinError(err error, meta map[string]any) error {
	if err == nil || len(meta) == 0 {
		return err
	}
	pin, _ := meta[farmPinMarkerMetadataKey].(string)
	if strings.TrimSpace(pin) == "" {
		return err
	}
	var authErr *coreauth.Error
	if !errors.As(err, &authErr) || authErr == nil {
		return err
	}
	switch strings.TrimSpace(authErr.Code) {
	case "auth_not_found", "auth_unavailable":
	default:
		return err
	}
	status := authErr.HTTPStatus
	if status <= 0 {
		status = http.StatusServiceUnavailable
	}
	message := strings.TrimSpace(authErr.Message)
	if message == "" {
		message = "no auth available"
	}
	return &coreauth.Error{
		Code:       farmPinAuthUnavailableCode,
		Message:    message + "; pinned farm account is unavailable and the request was fail-closed (no fallback to other accounts)",
		Retryable:  authErr.Retryable,
		HTTPStatus: status,
	}
}
