package management

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kilo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const kiloWebProvider = "kilo"

var newKiloWebClient = func(cfg *config.Config) *kilo.KiloAuth {
	return kilo.NewKiloAuthWithHTTPClient(ordinaryWebOAuthHTTPClient(cfg))
}

func (h *Handler) RequestKiloToken(c *gin.Context) {
	h.mu.Lock()
	cfg := h.cfg.CloneForRuntime()
	scope := oauthFileScope{owner: h, generation: h.fileOAuthGeneration}
	h.mu.Unlock()
	dir, err := fileOAuthDirectory(fileOAuthConfigDir(cfg))
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "credential directory unavailable"})
		return
	}
	client := newKiloWebClient(cfg)
	grant, err := client.InitiateDeviceFlow(c.Request.Context())
	if err != nil || grant == nil || strings.TrimSpace(grant.Code) == "" || strings.ContainsAny(grant.Code, "/\\?#") ||
		grant.Code == "." || grant.Code == ".." || !validKiloVerificationURL(grant.VerificationURL) {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Kilo did not return a valid authorization grant"})
		return
	}
	state, err := misc.GenerateRandomState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization state"})
		return
	}
	h.mu.Lock()
	currentDir, err := fileOAuthDirectory(fileOAuthConfigDir(h.cfg))
	if err != nil || currentDir != dir || scope.generation != h.fileOAuthGeneration {
		h.mu.Unlock()
		c.JSON(http.StatusConflict, gin.H{"error": "credential configuration changed"})
		return
	}
	// A device grant never writes a browser callback file.
	oauthSessions.registerBuiltinWithStorage(state, kiloWebProvider, "", "", dir, h, h.fileOAuthGeneration)
	h.mu.Unlock()

	go h.completeKiloGrant(state, grant, client)
	c.JSON(http.StatusOK, gin.H{
		"status": "ok", "url": grant.VerificationURL, "state": state,
		"user_code": grant.Code, "verification_uri": grant.VerificationURL,
	})
}

func validKiloVerificationURL(value string) bool {
	target, err := url.Parse(value)
	if err != nil || target.Scheme != "https" || target.User != nil || target.Hostname() == "" {
		return false
	}
	host := strings.ToLower(target.Hostname())
	return host == "kilo.ai" || strings.HasSuffix(host, ".kilo.ai") ||
		host == "kilocode.ai" || strings.HasSuffix(host, ".kilocode.ai")
}

func (h *Handler) completeKiloGrant(state string, grant *kilo.DeviceAuthResponse, client *kilo.KiloAuth) {
	timeout := 5 * time.Minute
	if grant.ExpiresIn > 0 && grant.ExpiresIn < int(timeout/time.Second) {
		timeout = time.Duration(grant.ExpiresIn) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	go watchOAuthSessionCancel(ctx, cancel, state, kiloWebProvider)
	status, err := client.PollForToken(ctx, grant.Code)
	if err != nil || status == nil || strings.TrimSpace(status.Token) == "" {
		if IsOAuthSessionPending(state, kiloWebProvider) {
			SetOAuthSessionError(state, "Kilo authorization did not complete")
		}
		return
	}
	if !IsOAuthSessionPending(state, kiloWebProvider) {
		return
	}
	email := strings.TrimSpace(status.UserEmail)
	orgID, model := "", ""
	if profile, err := client.GetProfile(ctx, status.Token); err == nil && profile != nil {
		if email == "" {
			email = strings.TrimSpace(profile.Email)
		}
		if len(profile.Orgs) != 0 {
			orgID = profile.Orgs[0].ID
		}
	}
	name := kilo.CredentialFileName(email)
	if email == "" || filepath.Base(name) != name || strings.ContainsAny(email, "\\/:\x00\r\n") {
		SetOAuthSessionError(state, "Kilo account identity is missing or invalid")
		return
	}
	if defaults, err := client.GetDefaults(ctx, status.Token, orgID); err == nil && defaults != nil {
		model = defaults.Model
	}

	record := &coreauth.Auth{ID: name, FileName: name, Label: email, Provider: kiloWebProvider,
		Metadata: map[string]any{"type": kiloWebProvider, "email": email,
			"kilocodeToken": status.Token, "kilocodeOrganizationId": orgID, "kilocodeModel": model,
		}}
	h.commitAndCompleteBuiltinOAuthSession(ctx, state, kiloWebProvider, "", record, "Failed to save Kilo credentials", true)
}
