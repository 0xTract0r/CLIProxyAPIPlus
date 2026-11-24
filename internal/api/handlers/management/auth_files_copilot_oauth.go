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
	h.requestCopilotDeviceOAuth(c, "", "")
}

func (h *Handler) requestCopilotDeviceOAuth(c *gin.Context, name, index string) {
	h.mu.Lock()
	cfg := h.cfg.CloneForRuntime()
	scope := oauthFileScope{owner: h, generation: h.fileOAuthGeneration}
	store := h.tokenStore
	h.mu.Unlock()
	dir, err := fileOAuthDirectory(fileOAuthConfigDir(cfg))
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "OAuth credential directory is not configured"})
		return
	}

	cfg.AuthDir = dir
	requestedProxy := oauthSessionProxyURLFromContext(c)
	route := strings.TrimSpace(cfg.ProxyURL)
	var selected oauthRecoveryTarget
	if name != "" || index != "" {
		if store == nil {
			store = sdkAuth.GetTokenStore()
		}
		if _, ok := store.(*sdkAuth.FileTokenStore); !ok {
			c.JSON(http.StatusConflict, gin.H{"error": "selected Copilot login requires local credential storage"})
			return
		}
		target, status, message := h.ordinaryAccountSettingsTarget(ordinaryAccountSettingsRequest{name: name, index: index})
		if status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}
		if !strings.EqualFold(strings.TrimSpace(target.auth.Provider), copilot.Provider) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "provider does not match target credential"})
			return
		}
		subject, savedRoute, errIdentity := target.file.CopilotIdentity()
		if errIdentity != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "selected Copilot file changed or has no verified GitHub account ID"})
			return
		}
		runtimeSubject, ok := sdkAuth.CopilotAccountID(target.auth.Metadata["account_id"])
		if !ok || runtimeSubject != subject {
			c.JSON(http.StatusConflict, gin.H{"error": "selected Copilot account identity is not current"})
			return
		}
		if runtimeRoute := strings.TrimSpace(target.auth.ProxyURL); runtimeRoute != "" && runtimeRoute != savedRoute {
			c.JSON(http.StatusConflict, gin.H{"error": "selected Copilot credential route is not current"})
			return
		}
		if savedRoute != "" {
			route = savedRoute
		}
		if requestedProxy != "" && requestedProxy != route {
			c.JSON(http.StatusBadRequest, gin.H{"error": "selected credential route does not match the requested route"})
			return
		}
		selected = oauthRecoveryTarget{
			AuthID: target.auth.ID, FileName: firstNonEmpty(target.auth.FileName, target.auth.ID),
			Path: authAttribute(target.auth, "path"), Provider: copilot.Provider, AccountID: subject,
			SelectedFileRevision: target.revision(), SelectedFile: target.file,
			SelectedRegistrationEpoch: target.auth.RegistrationEpoch,
			SelectedCredentialVersion: target.auth.CredentialVersion, SelectedDisabled: target.auth.Disabled,
			SelectedRouteInherits: savedRoute == "", SelectedGlobalRoute: route,
		}
	} else if requestedProxy != "" {
		route = requestedProxy
	}
	transport, mode, err := proxyutil.BuildHTTPTransport(route)
	if err != nil || (mode == proxyutil.ModeProxy && !proxyutil.ValidRequestProxy(route)) || (mode != proxyutil.ModeInherit && transport == nil) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "invalid OAuth proxy configuration"})
		return
	}
	h.mu.Lock()
	current := h.copilotOAuthScopeCurrentLocked(scope, dir)
	h.mu.Unlock()
	if !current || c.Request.Context().Err() != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "OAuth credential scope changed"})
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
	if selected.AuthID != "" {
		SetOAuthSessionTarget(state, selected)
	}
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
		if selected.AuthID != "" {
			h.mu.Unlock()
			subject, ok := sdkAuth.CopilotAccountID(record.Metadata["account_id"])
			if !ok || subject != selected.AccountID {
				SetOAuthSessionError(state, "Authenticated account does not match target credential")
				return
			}
			h.commitAndCompleteBuiltinOAuthSession(ctx, state, copilot.Provider, "", record, "Failed to save token to file", false)
			return
		}
		applySessionProxyToAuthRecord(record, requestedProxy)
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
	reply := gin.H{
		"status": "ok", "state": state, "flow": "device", "url": device.VerificationURI,
		"verification_uri": device.VerificationURI, "user_code": device.UserCode, "expires_in": device.ExpiresIn,
	}
	if selected.AuthID != "" {
		reply["target_auth_name"] = selected.FileName
	}
	c.JSON(http.StatusOK, reply)
}

// copilotOAuthScopeCurrentLocked checks the exact owner and resolved directory.
func (h *Handler) copilotOAuthScopeCurrentLocked(scope oauthFileScope, dir string) bool {
	if h != scope.owner || h.fileOAuthGeneration != scope.generation {
		return false
	}
	current, err := fileOAuthDirectory(fileOAuthConfigDir(h.cfg))
	return err == nil && current == dir
}
