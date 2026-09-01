package management

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// proxyConnectivityProbeRequest is the request body for the proxy connectivity
// probe endpoint. Only a proxy_url is accepted; the probe target is never taken
// from the request (hardcoded neutral targets below) to avoid SSRF.
type proxyConnectivityProbeRequest struct {
	ProxyURL string `json:"proxy_url"`
}

// proxyConnectivityProbeResult is the response body. It intentionally never
// echoes the proxy_url (which may contain credentials); exit_ip is only set on
// success.
type proxyConnectivityProbeResult struct {
	OK     bool   `json:"ok"`
	ExitIP string `json:"exit_ip"`
	Reason string `json:"reason"`
}

// Strict reason set surfaced to the frontend. Do not add values outside this
// set without updating the HTTP contract in the frontend.
const (
	proxyProbeReasonOK          = "ok"
	proxyProbeReasonEmpty       = "empty_proxy_url"
	proxyProbeReasonInvalid     = "invalid_proxy_url"
	proxyProbeReasonDialFailed  = "dial_failed"
	proxyProbeReasonTimeout     = "timeout"
	proxyProbeReasonProbeFailed = "probe_failed"
)

// proxyConnectivityProbeTargets are neutral echo-IP endpoints. They are
// hardcoded (never sourced from the request) so this endpoint cannot be turned
// into an SSRF gadget. The list is tried in order; the first success wins.
var proxyConnectivityProbeTargets = []string{
	"https://api.ipify.org",
	"https://ifconfig.me/ip",
	"https://icanhazip.com",
}

const (
	// proxyProbePerTargetTimeout bounds a single target attempt.
	proxyProbePerTargetTimeout = 8 * time.Second
	// proxyProbeOverallTimeout bounds the whole probe across all targets.
	proxyProbeOverallTimeout = 20 * time.Second
	// proxyProbeBodyLimit caps how much of the echo-IP body we read.
	proxyProbeBodyLimit = 1 << 16
)

// runProxyConnectivityProbe is the package-level seam the handler calls. Tests
// stub this to exercise the success path without real network I/O.
var runProxyConnectivityProbe = probeProxyConnectivity

// probeProxyExitIP is the inner network seam. Format-rejection paths never reach
// it, so tests can assert "no network was sent" for empty/invalid/direct inputs
// by stubbing this to fail if invoked.
var probeProxyExitIP = dialProxyExitIP

// RunProxyConnectivityProbe validates a proxy_url and, if it is a concrete proxy,
// dials a neutral target through it and reports the observed exit IP. The format
// check happens first and performs no network I/O for empty/invalid/direct
// values. No account token is ever attached, and the proxy_url is never echoed
// back or logged in cleartext.
func (h *Handler) RunProxyConnectivityProbe(c *gin.Context) {
	var req proxyConnectivityProbeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	raw := strings.TrimSpace(req.ProxyURL)

	var ctx context.Context
	if c.Request != nil {
		ctx = c.Request.Context()
	} else {
		ctx = context.Background()
	}

	result := runProxyConnectivityProbe(ctx, raw)
	c.JSON(http.StatusOK, result)
}

// probeProxyConnectivity classifies the proxy setting and, only for a concrete
// proxy, runs the network probe. It recovers from panics into probe_failed so a
// diagnostic never crashes the management API.
func probeProxyConnectivity(ctx context.Context, raw string) (result proxyConnectivityProbeResult) {
	defer func() {
		if r := recover(); r != nil {
			log.Warnf("proxy connectivity probe panicked proxy=%s: %v", proxyutil.Redact(raw), r)
			result = proxyConnectivityProbeResult{OK: false, Reason: proxyProbeReasonProbeFailed}
		}
	}()

	log.Debugf("proxy connectivity probe requested proxy=%s", proxyutil.Redact(raw))

	setting, _ := proxyutil.Parse(raw)
	switch setting.Mode {
	case proxyutil.ModeInherit:
		// Empty / unset: not a proxy, do not dial.
		return proxyConnectivityProbeResult{OK: false, Reason: proxyProbeReasonEmpty}
	case proxyutil.ModeInvalid:
		// Malformed / unsupported: do not dial.
		return proxyConnectivityProbeResult{OK: false, Reason: proxyProbeReasonInvalid}
	case proxyutil.ModeDirect:
		// direct/none is not a residential proxy; the probe validates real
		// proxies, so reject it as invalid rather than dialing directly.
		return proxyConnectivityProbeResult{OK: false, Reason: proxyProbeReasonInvalid}
	case proxyutil.ModeProxy:
		// fall through to the network probe below.
	default:
		return proxyConnectivityProbeResult{OK: false, Reason: proxyProbeReasonInvalid}
	}

	transport, mode, errBuild := proxyutil.BuildHTTPTransport(raw)
	if errBuild != nil || mode != proxyutil.ModeProxy || transport == nil {
		log.Warnf("proxy connectivity probe transport build failed proxy=%s: %v", proxyutil.Redact(raw), errBuild)
		return proxyConnectivityProbeResult{OK: false, Reason: proxyProbeReasonInvalid}
	}

	client := &http.Client{Transport: transport}
	return probeProxyExitIP(ctx, client, raw)
}

// dialProxyExitIP tries each neutral target through the proxy client and returns
// the first observed exit IP. It never carries an Authorization or
// Proxy-Authorization header sourced from an account. On total failure it maps
// to timeout (if any attempt or the overall deadline expired) or dial_failed.
func dialProxyExitIP(ctx context.Context, client *http.Client, raw string) proxyConnectivityProbeResult {
	if ctx == nil {
		ctx = context.Background()
	}
	overallCtx, cancelOverall := context.WithTimeout(ctx, proxyProbeOverallTimeout)
	defer cancelOverall()

	sawTimeout := false
	for _, target := range proxyConnectivityProbeTargets {
		ip, errAttempt, timedOut := probeSingleProxyTarget(overallCtx, client, target)
		if errAttempt == nil && ip != "" {
			return proxyConnectivityProbeResult{OK: true, ExitIP: ip, Reason: proxyProbeReasonOK}
		}
		if timedOut {
			sawTimeout = true
		}
		log.Debugf("proxy connectivity probe target failed proxy=%s: %v", proxyutil.Redact(raw), errAttempt)
		if overallCtx.Err() != nil {
			sawTimeout = true
			break
		}
	}

	if sawTimeout {
		return proxyConnectivityProbeResult{OK: false, Reason: proxyProbeReasonTimeout}
	}
	return proxyConnectivityProbeResult{OK: false, Reason: proxyProbeReasonDialFailed}
}

// probeSingleProxyTarget performs one bounded GET through the proxy client and
// returns the trimmed exit IP. The bool reports whether the failure was a
// timeout so the caller can map the reason precisely.
func probeSingleProxyTarget(ctx context.Context, client *http.Client, target string) (string, error, bool) {
	attemptCtx, cancel := context.WithTimeout(ctx, proxyProbePerTargetTimeout)
	defer cancel()

	req, errReq := http.NewRequestWithContext(attemptCtx, http.MethodGet, target, nil)
	if errReq != nil {
		return "", errReq, false
	}
	// Never attach any token to the neutral probe request.
	req.Header.Del("Authorization")
	req.Header.Del("Proxy-Authorization")

	resp, errDo := client.Do(req)
	if errDo != nil {
		return "", errDo, isTimeoutErr(attemptCtx, errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("proxy connectivity probe body close failed: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode), false
	}

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, proxyProbeBodyLimit))
	if errRead != nil {
		return "", errRead, isTimeoutErr(attemptCtx, errRead)
	}
	ip := strings.TrimSpace(string(body))
	if ip == "" {
		return "", errors.New("empty exit IP body"), false
	}
	return ip, nil, false
}

// isTimeoutErr reports whether the failure was caused by a deadline expiring.
func isTimeoutErr(ctx context.Context, err error) bool {
	if ctx != nil && ctx.Err() == context.DeadlineExceeded {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}
