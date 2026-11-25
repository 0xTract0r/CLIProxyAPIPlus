package helps

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var errLegacyTLSTransport = errors.New("legacy TLS profile is unsupported in the standard transport build")

// NewUtlsHTTPClient rejects calls still wired to the retired transport.
// Ordinary protocol callers select NewProxyAwareHTTPClient explicitly.
func NewUtlsHTTPClient(_ context.Context, _ *config.Config, _ *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: deniedHTTPTransport{errLegacyTLSTransport}}
}
