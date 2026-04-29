package helps

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// RuntimeTransportProfile describes the minimal runtime transport contract
// derived from account_settings.transport_profile.
type RuntimeTransportProfile struct {
	Provider  string
	Family    string
	ProfileID string
	ALPN      []string
}

func ResolveRuntimeTransportProfile(auth *cliproxyauth.Auth) *RuntimeTransportProfile {
	if auth == nil || len(auth.Metadata) == 0 {
		return nil
	}

	settings := normalizeObject(auth.Metadata["account_settings"])
	if len(settings) == 0 {
		return nil
	}
	profileRaw, ok := settings["transport_profile"]
	if !ok || profileRaw == nil {
		return nil
	}
	profileMap := normalizeObject(profileRaw)
	if len(profileMap) == 0 {
		return nil
	}

	profileID := firstNonEmptyString(profileMap["profile_id"], profileMap["preset"])
	if profileID == "" {
		return nil
	}

	provider := strings.ToLower(strings.TrimSpace(firstNonEmptyString(profileMap["provider"])))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(auth.Provider))
	}
	family := strings.ToLower(strings.TrimSpace(firstNonEmptyString(profileMap["family"])))
	if family == "" && provider == "claude" {
		family = "utls"
	} else if family == "" && provider == "codex" {
		family = "standard"
	}

	profile := &RuntimeTransportProfile{
		Provider:  provider,
		Family:    family,
		ProfileID: strings.ToLower(strings.TrimSpace(profileID)),
		ALPN:      normalizeStringSlice(profileMap["alpn"]),
	}
	return profile
}

func IsRuntimeTransportProfileEnforced(auth *cliproxyauth.Auth) bool {
	profile := ResolveRuntimeTransportProfile(auth)
	return profile != nil && profile.SupportsRuntime()
}

func RuntimeTransportProfileCacheKey(proxyURL string, auth *cliproxyauth.Auth) string {
	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil || !profile.SupportsRuntime() || auth == nil {
		return ""
	}

	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		authID = strings.TrimSpace(auth.FileName)
	}
	if authID == "" {
		authID = strings.TrimSpace(auth.Label)
	}
	if authID == "" {
		authID = "anonymous"
	}

	return fmt.Sprintf(
		"transport:%s|%s|%s|%s",
		profile.Provider,
		authID,
		strings.TrimSpace(proxyURL),
		profile.cacheToken(),
	)
}

func BuildRuntimeTransportRoundTripper(proxyURL string, auth *cliproxyauth.Auth) (http.RoundTripper, bool) {
	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil || !profile.SupportsRuntime() {
		return nil, false
	}

	switch profile.Provider {
	case "claude":
		return NewUtlsRoundTripperForProfile(proxyURL, profile.ProfileID), true
	case "codex":
		return NewCodexTransportRoundTripperForProfile(proxyURL, profile.ProfileID, profile.ALPN), true
	default:
		return nil, false
	}
}

func (p *RuntimeTransportProfile) SupportsRuntime() bool {
	if p == nil {
		return false
	}
	switch p.Provider {
	case "claude":
		if p.Family != "" && p.Family != "utls" {
			return false
		}
		switch p.ProfileID {
		case "provider-default",
			"claude_chrome_like_mac_v1",
			"claude_chrome_like_mac_v2",
			"claude_chrome_like_mac_v3",
			"chrome_120",
			"chrome_131",
			"chrome_133":
			return true
		default:
			return false
		}
	case "codex":
		if p.Family != "" && p.Family != "standard" {
			return false
		}
		switch p.ProfileID {
		case "provider-default",
			"codex_isolated_transport_v1",
			"codex_managed_transport_v1":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func (p *RuntimeTransportProfile) cacheToken() string {
	if p == nil {
		return ""
	}
	alpn := append([]string(nil), p.ALPN...)
	sort.Strings(alpn)
	return fmt.Sprintf("%s|%s|%s", p.Family, p.ProfileID, strings.Join(alpn, ","))
}

func NewCodexTransportRoundTripperForProfile(proxyURL string, profileID string, alpn []string) http.RoundTripper {
	_ = profileID

	base := standardTransportForProxy(proxyURL)
	transport, ok := base.(*http.Transport)
	if !ok || transport == nil {
		return base
	}

	cloned := transport.Clone()
	cloned.ForceAttemptHTTP2 = true
	cloned.MaxIdleConnsPerHost = 4
	cloned.MaxIdleConns = 16
	if len(alpn) > 0 {
		if cloned.TLSClientConfig == nil {
			cloned.TLSClientConfig = &tls.Config{}
		}
		cloned.TLSClientConfig.NextProtos = append([]string(nil), alpn...)
		cloned.ForceAttemptHTTP2 = containsStringFold(alpn, "h2")
	}
	return cloned
}

func containsStringFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}

func normalizeObject(raw any) map[string]any {
	if raw == nil {
		return nil
	}
	switch value := raw.(type) {
	case map[string]any:
		if len(value) == 0 {
			return nil
		}
		return value
	case map[string]string:
		if len(value) == 0 {
			return nil
		}
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[key] = item
		}
		return out
	default:
		data, errMarshal := json.Marshal(raw)
		if errMarshal != nil || len(data) == 0 {
			return nil
		}
		var out map[string]any
		if errUnmarshal := json.Unmarshal(data, &out); errUnmarshal != nil || len(out) == 0 {
			return nil
		}
		return out
	}
}

func firstNonEmptyString(values ...any) string {
	for _, raw := range values {
		if text, ok := raw.(string); ok {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func normalizeStringSlice(raw any) []string {
	switch value := raw.(type) {
	case []string:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok {
				if trimmed := strings.TrimSpace(text); trimmed != "" {
					out = append(out, trimmed)
				}
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return nil
	}
}
