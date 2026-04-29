package cliproxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// defaultRoundTripperProvider returns a per-auth HTTP RoundTripper based on
// the Auth.ProxyURL value. It caches transports per proxy URL string.
type defaultRoundTripperProvider struct {
	mu    sync.RWMutex
	cache map[string]http.RoundTripper
}

func newDefaultRoundTripperProvider() *defaultRoundTripperProvider {
	return &defaultRoundTripperProvider{cache: make(map[string]http.RoundTripper)}
}

// RoundTripperFor implements coreauth.RoundTripperProvider.
func (p *defaultRoundTripperProvider) RoundTripperFor(auth *coreauth.Auth) http.RoundTripper {
	if auth == nil {
		return nil
	}
	proxyStr := strings.TrimSpace(auth.ProxyURL)
	if proxyStr == "" {
		return nil
	}
	cacheKey := roundTripperCacheKey(auth, proxyStr)
	p.mu.RLock()
	rt := p.cache[cacheKey]
	p.mu.RUnlock()
	if rt != nil {
		return rt
	}
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyStr)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
		return nil
	}
	if transport == nil {
		return nil
	}
	p.mu.Lock()
	p.cache[cacheKey] = transport
	p.mu.Unlock()
	return transport
}

func roundTripperCacheKey(auth *coreauth.Auth, proxyStr string) string {
	if auth == nil {
		return proxyStr
	}
	return fmt.Sprintf(
		"%s|%s|%s|%s",
		proxyStr,
		strings.ToLower(strings.TrimSpace(auth.Provider)),
		roundTripperAuthIdentity(auth),
		roundTripperTransportProfileToken(auth),
	)
}

func roundTripperAuthIdentity(auth *coreauth.Auth) string {
	if auth == nil {
		return "anonymous"
	}
	for _, value := range []string{auth.ID, auth.FileName, auth.Label} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return "anonymous"
}

func roundTripperTransportProfileToken(auth *coreauth.Auth) string {
	if auth == nil || len(auth.Metadata) == 0 {
		return ""
	}
	accountSettings, ok := auth.Metadata["account_settings"].(map[string]any)
	if !ok || len(accountSettings) == 0 {
		return ""
	}
	profileRaw, ok := accountSettings["transport_profile"]
	if !ok || profileRaw == nil {
		return ""
	}
	data, errMarshal := json.Marshal(profileRaw)
	if errMarshal != nil || len(data) == 0 {
		return ""
	}
	return string(data)
}
