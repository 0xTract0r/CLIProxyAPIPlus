package management

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/copilot"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
)

type copilotDeviceGrant struct {
	start    func(context.Context, *http.Client) (copilot.DeviceCode, error)
	complete func(context.Context, *http.Client, copilot.DeviceCode) (*coreauth.Auth, error)
}

// RequestCopilotDeviceOAuth starts ordinary Copilot device login on the legacy endpoint.
func (h *Handler) RequestCopilotDeviceOAuth(c *gin.Context) {
	for _, key := range []string{"name", "target_auth_id", "auth_id", "auth_index", "auth_name", "proxy_url", "proxy-url"} {
		if strings.TrimSpace(c.Query(key)) != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Copilot device login does not accept a target credential or request proxy"})
			return
		}
	}
	h.mu.Lock()
	cfg := h.cfg.CloneForRuntime()
	scope := oauthFileScope{owner: h, generation: h.fileOAuthGeneration}
	h.mu.Unlock()
	dir, err := fileOAuthDirectory(fileOAuthConfigDir(cfg))
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "OAuth credential directory is not configured"})
		return
	}

	cfg.AuthDir = dir
	transport, mode, err := proxyutil.BuildHTTPTransport(cfg.ProxyURL)
	if err != nil || (mode == proxyutil.ModeProxy && !proxyutil.ValidRequestProxy(cfg.ProxyURL)) || (mode != proxyutil.ModeInherit && transport == nil) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "invalid OAuth proxy configuration"})
		return
	}
	client := &http.Client{Timeout: 30 * time.Second}
	if transport != nil {
		client.Transport = transport
	}
	grant := h.copilotDeviceGrant
	if grant == nil {
		grant = &copilotDeviceGrant{start: copilot.StartDeviceFlow, complete: sdkAuth.CompleteCopilotDeviceLogin}
	}
	state, err := misc.GenerateRandomState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate OAuth state"})
		return
	}
	device, err := grant.start(c.Request.Context(), client)
	if err != nil || device.ExpiresIn <= 0 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to start device authorization flow"})
		return
	}
	device.ExpiresIn = min(device.ExpiresIn, int(oauthSessionTTL/time.Second))
	h.mu.Lock()
	if !h.copilotOAuthScopeCurrentLocked(scope, dir) || c.Request.Context().Err() != nil {
		h.mu.Unlock()
		c.JSON(http.StatusConflict, gin.H{"error": "OAuth credential scope changed"})
		return
	}
	oauthSessions.registerBuiltin(state, copilot.Provider, "", dir, h, scope.generation)
	h.mu.Unlock()

	ctx := PopulateAuthContext(context.Background(), c)
	go func() {
		pollCtx, cancel := context.WithTimeout(ctx, time.Duration(device.ExpiresIn)*time.Second)
		defer cancel()
		go watchOAuthSessionCancel(pollCtx, cancel, state, copilot.Provider)
		record, loginErr := grant.complete(pollCtx, client, device)
		h.mu.Lock()
		current := h.copilotOAuthScopeCurrentLocked(scope, dir)
		if !current {
			h.mu.Unlock()
			oauthSessions.cancel(state, &scope)
			return
		}
		if loginErr != nil || record == nil || !strings.EqualFold(record.Provider, copilot.Provider) || pollCtx.Err() != nil {
			h.mu.Unlock()
			SetOAuthSessionError(state, "Authentication failed")
			return
		}
		commitErr := claimOAuthSessionCommit(state, copilot.Provider)
		h.mu.Unlock()
		if commitErr != nil {
			return
		}
		// The claim is the commit boundary. Use its immutable config snapshot even
		// when a later reload changes the handler's directory.
		if _, errSave := h.saveTokenRecordWithConfig(ctx, record, cfg); errSave != nil {
			FailOAuthSessionCommit(state, "Failed to save token to file")
			return
		}
		CompleteOAuthSession(state)
	}()
	c.JSON(http.StatusOK, gin.H{
		"status": "ok", "state": state, "flow": "device", "url": device.VerificationURI,
		"verification_uri": device.VerificationURI, "user_code": device.UserCode, "expires_in": device.ExpiresIn,
	})
}

// copilotOAuthScopeCurrentLocked checks the exact owner and resolved directory.
func (h *Handler) copilotOAuthScopeCurrentLocked(scope oauthFileScope, dir string) bool {
	if h != scope.owner || h.fileOAuthGeneration != scope.generation {
		return false
	}
	current, err := fileOAuthDirectory(fileOAuthConfigDir(h.cfg))
	return err == nil && current == dir
}
