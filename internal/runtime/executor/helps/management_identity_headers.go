package helps

import (
	"net/http"
	"strings"
)

// managementIdentityExcludedHeaders lists headers a serving header builder sets
// that are NOT client identity: credentials, content negotiation, transport
// control, beta flags and per-request / per-conversation identifiers. The
// management api-call proxy keeps the dashboard-supplied values for these, so
// they are removed from the managed identity set (compared case-insensitively).
var managementIdentityExcludedHeaders = map[string]struct{}{
	"authorization":            {},
	"x-api-key":                {},
	"content-type":             {},
	"content-length":           {},
	"accept":                   {},
	"accept-encoding":          {},
	"anthropic-beta":           {},
	"connection":               {},
	"host":                     {},
	"x-client-request-id":      {},
	"x-claude-code-session-id": {},
	"chatgpt-account-id":       {},
	"session-id":               {},
	"session_id":               {},
	"x-codex-turn-metadata":    {},
}

// LogAccountEgressBlocked emits the same redacted egress_blocked warning the
// serving guard uses (reason, masked auth ID, site; never the proxy URL or any
// credential) for callers outside this package, such as the management
// api-call fail-closed gate.
func LogAccountEgressBlocked(reason, authID, site string) {
	logEgressBlocked(reason, authID, site)
}

// ManagementIdentityHeaders filters a header set produced by a provider's
// serving header builder down to the client identity headers (User-Agent,
// X-App, X-Stainless-*, Originator, client hints, ...) that a management
// request must present so it matches the account's serving identity. It
// returns nil when no User-Agent is present, because an identity without a UA
// cannot be presented consistently.
func ManagementIdentityHeaders(src http.Header) http.Header {
	if len(src) == 0 || strings.TrimSpace(src.Get("User-Agent")) == "" {
		return nil
	}
	out := make(http.Header, len(src))
	for name, values := range src {
		if _, excluded := managementIdentityExcludedHeaders[strings.ToLower(strings.TrimSpace(name))]; excluded {
			continue
		}
		kept := make([]string, 0, len(values))
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				kept = append(kept, value)
			}
		}
		if len(kept) > 0 {
			out[name] = kept
		}
	}
	return out
}
