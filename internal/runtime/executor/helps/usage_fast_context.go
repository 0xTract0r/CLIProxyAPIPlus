package helps

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

// SetCodexFastContext captures the final outbound request, never response tier echoes.
// Keeping provenance inside telemetry makes realtime and historical exports identical.
func (r *UsageReporter) SetCodexFastContext(client, outbound []byte, enabled bool) {
	if r == nil {
		return
	}
	clientTier := strings.ToLower(strings.TrimSpace(gjson.GetBytes(client, "service_tier").String()))
	upstreamTier := strings.ToLower(strings.TrimSpace(gjson.GetBytes(outbound, "service_tier").String()))
	if upstreamTier == "" {
		upstreamTier = "auto"
	}
	source := "unknown"
	switch upstreamTier {
	case "priority":
		if enabled && clientTier == "priority" {
			source = "both"
		} else if enabled {
			source = "account"
		} else if clientTier == "priority" {
			source = "client"
		}
	case "default", "auto":
		source = "default"
	case "flex":
		if clientTier == "flex" {
			source = "client"
		}
	}
	kind := "serving"
	if !r.generate || gjson.GetBytes(client, "generate").Type == gjson.False {
		kind = "prewarm"
	}
	r.telemetryMu.Lock()
	defer r.telemetryMu.Unlock()
	r.telemetry.Version = 2
	r.telemetry.VisibleContentObserved = true
	r.telemetry.FastContext = &usage.FastContext{SchemaVersion: 1, ClientServiceTier: clientTier, UpstreamRequestServiceTier: upstreamTier, ServerFastEnabled: &enabled, TierSource: source, RequestKind: kind}
}
