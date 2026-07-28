package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// PatchAuthFileStatus toggles the disabled state of an auth file
func (h *Handler) PatchAuthFileStatus(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name      string `json:"name"`
		AuthIndex string `json:"auth_index"`
		Disabled  *bool  `json:"disabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(req.Name)
	authIndex := strings.TrimSpace(req.AuthIndex)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if req.Disabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "disabled is required"})
		return
	}

	ctx := c.Request.Context()

	targetAuth, _ := h.lookupAuthFile(name, authIndex)
	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if coreauth.IsPluginVirtualAuth(targetAuth) {
		// Allow status changes only when targeting the source auth file name, matching delete semantics.
		// Expanded virtual project auths still cannot be modified independently.
		if !isPluginVirtualSourceDelete(name, targetAuth) {
			c.JSON(http.StatusConflict, gin.H{"error": errPluginVirtualAuth.Error()})
			return
		}
		if errPatch := h.patchPluginVirtualSourceStatus(ctx, targetAuth, *req.Disabled); errPatch != nil {
			status := http.StatusInternalServerError
			if errors.Is(errPatch, errAuthFileNotFound) || os.IsNotExist(errPatch) {
				status = http.StatusNotFound
			}
			c.JSON(status, gin.H{"error": errPatch.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "disabled": *req.Disabled})
		return
	}

	if coreauth.IsConfigAPIKeyAuth(targetAuth) {
		h.mu.Lock()
		handled, errToggle := toggleConfigAPIKeyExcludedAll(h.cfg, targetAuth, *req.Disabled)
		if errToggle != nil {
			h.mu.Unlock()
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update config api key: %v", errToggle)})
			return
		}
		if !handled {
			h.mu.Unlock()
			c.JSON(http.StatusNotFound, gin.H{"error": "config api key entry not found"})
			return
		}
		cfgSnapshot, okSnapshot := h.saveConfigAndSnapshotLocked(c)
		h.mu.Unlock()
		if !okSnapshot {
			return
		}
		h.reloadConfigAfterManagementSave(ctx, cfgSnapshot)
		if h.tokenStore != nil {
			_ = h.tokenStore.Delete(ctx, targetAuth.ID)
		}
		c.JSON(http.StatusOK, gin.H{
			"status":           "ok",
			"disabled":         *req.Disabled,
			"via":              "config:excluded-models",
			"excluded_pattern": configAPIKeyDisablePattern,
		})
		return
	}

	applyAuthDisabledState(targetAuth, *req.Disabled)
	if _, err := h.authManager.Update(ctx, targetAuth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "disabled": *req.Disabled})
}

// patchPluginVirtualSourceStatus toggles disabled on a plugin multi-auth source file and all
// runtime auths expanded from it. Virtual project children cannot be toggled independently.
func (h *Handler) patchPluginVirtualSourceStatus(ctx context.Context, targetAuth *coreauth.Auth, disabled bool) error {
	if h == nil || h.authManager == nil || targetAuth == nil {
		return fmt.Errorf("core auth manager unavailable")
	}
	sourcePath := strings.TrimSpace(authAttribute(targetAuth, coreauth.AttributeVirtualSource))
	if sourcePath == "" {
		sourcePath = strings.TrimSpace(authAttribute(targetAuth, "path"))
	}
	if sourcePath == "" {
		return errPluginVirtualAuth
	}
	if errWrite := setSourceAuthFileDisabled(sourcePath, disabled); errWrite != nil {
		if os.IsNotExist(errWrite) {
			return errAuthFileNotFound
		}
		return fmt.Errorf("failed to update source auth file: %w", errWrite)
	}
	now := time.Now()
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		if !sameAuthFilePath(authAttribute(auth, "path"), sourcePath) &&
			!sameAuthFilePath(authAttribute(auth, coreauth.AttributeVirtualSource), sourcePath) {
			continue
		}
		applyAuthDisabledState(auth, disabled)
		auth.UpdatedAt = now
		if _, errUpdate := h.authManager.Update(ctx, auth); errUpdate != nil {
			return fmt.Errorf("failed to update auth %s: %w", auth.ID, errUpdate)
		}
	}
	return nil
}

func setSourceAuthFileDisabled(path string, disabled bool) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("source auth path is empty")
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return errRead
	}
	metadata := make(map[string]any)
	if len(bytes.TrimSpace(data)) > 0 {
		if errUnmarshal := json.Unmarshal(data, &metadata); errUnmarshal != nil {
			return fmt.Errorf("invalid auth file: %w", errUnmarshal)
		}
	}
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata["disabled"] = disabled
	raw, errMarshal := json.Marshal(metadata)
	if errMarshal != nil {
		return fmt.Errorf("marshal auth file: %w", errMarshal)
	}
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		return errWrite
	}
	return nil
}

func applyAuthDisabledState(auth *coreauth.Auth, disabled bool) {
	if auth == nil {
		return
	}
	auth.Disabled = disabled
	if disabled {
		auth.Status = coreauth.StatusDisabled
		auth.StatusMessage = "disabled via management API"
	} else {
		auth.Status = coreauth.StatusActive
		auth.StatusMessage = ""
		// Fork recovery: re-enabling via management API must clear in-memory
		// cooldown so the auth becomes immediately selectable; otherwise prior
		// 429-driven ModelStates.Quota would keep the selector returning
		// model_cooldown.
		auth.Unavailable = false
		auth.NextRetryAfter = time.Time{}
		auth.Quota = coreauth.QuotaState{}
		auth.LastError = nil
		for _, ms := range auth.ModelStates {
			if ms == nil {
				continue
			}
			ms.Unavailable = false
			ms.NextRetryAfter = time.Time{}
			ms.Quota = coreauth.QuotaState{}
			ms.LastError = nil
			if ms.Status == coreauth.StatusError {
				ms.Status = coreauth.StatusActive
			}
		}
		// T3: an explicit operator re-enable is also a valid recovery signal for
		// core's automatic terminal-auth quarantine (see markAutoQuarantine /
		// clearAutoQuarantine): the operator is choosing to give the credential
		// another chance, so lift the lock now instead of leaving it unselectable.
		auth.ClearAutoQuarantine()
	}
	auth.UpdatedAt = time.Now()
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["disabled"] = disabled
}

// PatchAuthFileFields updates arbitrary metadata fields of an auth file.
func (h *Handler) PatchAuthFileFields(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req map[string]json.RawMessage
	decoder := json.NewDecoder(c.Request.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	nameRaw, ok := req["name"]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	var nameValue string
	if err := json.Unmarshal(nameRaw, &nameValue); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	name := strings.TrimSpace(nameValue)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	delete(req, "name")

	ctx := c.Request.Context()

	// Find auth by name or ID
	var targetAuth *coreauth.Auth
	if auth, ok := h.authManager.GetByID(name); ok {
		targetAuth = auth
	} else {
		auths := h.authManager.List()
		for _, auth := range auths {
			if auth.FileName == name {
				targetAuth = auth
				break
			}
		}
	}

	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if coreauth.IsPluginVirtualAuth(targetAuth) {
		c.JSON(http.StatusConflict, gin.H{"error": errPluginVirtualAuth.Error()})
		return
	}

	changed := false
	touchedRoots := make(map[string]struct{}, len(req))
	for key, rawValue := range req {
		fieldPath := strings.TrimSpace(key)
		if fieldPath == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "field name is required"})
			return
		}
		value, errDecode := decodeAuthFileFieldValue(rawValue)
		if errDecode != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid field %s", fieldPath)})
			return
		}
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}

		if fieldPath == "headers" {
			applyAuthFileHeadersPatch(targetAuth, value)
		} else if fieldPath == "claude_device_id" {
			// Fork anti-corr: the per-account synthetic Claude device_id is
			// operator-editable but must stay a well-formed UUID so it keeps
			// masking the real machine device_id across accounts.
			if errValidate := validateAuthFileClaudeDeviceIDPatch(value); errValidate != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
				return
			}
			if errSet := setAuthFileMetadataValue(targetAuth.Metadata, fieldPath, value); errSet != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": errSet.Error()})
				return
			}
		} else if errSet := setAuthFileMetadataValue(targetAuth.Metadata, fieldPath, value); errSet != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errSet.Error()})
			return
		}
		if root := rootAuthFileField(fieldPath); root != "" {
			touchedRoots[root] = struct{}{}
		}
		changed = true
	}
	if changed {
		syncAuthFileMetadataFields(targetAuth, touchedRoots)
	}

	if !changed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	targetAuth.UpdatedAt = time.Now()

	if _, err := h.authManager.Update(ctx, targetAuth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func decodeAuthFileFieldValue(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func rootAuthFileField(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if idx := strings.Index(path, "."); idx >= 0 {
		return strings.TrimSpace(path[:idx])
	}
	return path
}

func setAuthFileMetadataValue(metadata map[string]any, path string, value any) error {
	if metadata == nil {
		return fmt.Errorf("metadata is nil")
	}
	parts := strings.Split(path, ".")
	current := metadata
	for i, rawPart := range parts {
		part := strings.TrimSpace(rawPart)
		if part == "" {
			return fmt.Errorf("invalid field path: %s", path)
		}
		if i == len(parts)-1 {
			current[part] = value
			return nil
		}
		next, ok := current[part].(map[string]any)
		if !ok {
			next = make(map[string]any)
			current[part] = next
		}
		current = next
	}
	return nil
}

func applyAuthFileHeadersPatch(auth *coreauth.Auth, value any) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	headersPatch, ok := authFileHeadersStringMap(value)
	if !ok {
		auth.Metadata["headers"] = value
		return
	}

	existingHeaders := coreauth.ExtractCustomHeadersFromMetadata(auth.Metadata)
	nextHeaders := make(map[string]string, len(existingHeaders))
	for key, val := range existingHeaders {
		nextHeaders[key] = val
	}
	for key, value := range headersPatch {
		name := strings.TrimSpace(key)
		if name == "" {
			continue
		}
		val := strings.TrimSpace(value)
		if val == "" {
			delete(nextHeaders, name)
			continue
		}
		nextHeaders[name] = val
	}

	if len(nextHeaders) == 0 {
		delete(auth.Metadata, "headers")
		return
	}
	metaHeaders := make(map[string]any, len(nextHeaders))
	for key, value := range nextHeaders {
		metaHeaders[key] = value
	}
	auth.Metadata["headers"] = metaHeaders
}

func authFileHeadersStringMap(value any) (map[string]string, bool) {
	switch typed := value.(type) {
	case map[string]string:
		return typed, true
	case map[string]any:
		out := make(map[string]string, len(typed))
		for key, rawValue := range typed {
			value, ok := rawValue.(string)
			if !ok {
				return nil, false
			}
			out[key] = value
		}
		return out, true
	default:
		return nil, false
	}
}

func syncAuthFileMetadataFields(auth *coreauth.Auth, touchedRoots map[string]struct{}) {
	if auth == nil || len(touchedRoots) == 0 {
		return
	}
	if _, ok := touchedRoots["prefix"]; ok {
		if prefix, okString := auth.Metadata["prefix"].(string); okString {
			auth.Prefix = strings.TrimSpace(prefix)
		}
	}
	if _, ok := touchedRoots["proxy_url"]; ok {
		if proxyURL, okString := auth.Metadata["proxy_url"].(string); okString {
			auth.ProxyURL = strings.TrimSpace(proxyURL)
		}
	}
	if _, ok := touchedRoots["headers"]; ok {
		syncAuthFileHeaderAttributes(auth)
	}
	if _, ok := touchedRoots["priority"]; ok {
		syncAuthFilePriorityAttribute(auth)
	}
	if _, ok := touchedRoots["note"]; ok {
		syncAuthFileNoteAttribute(auth)
	}
	if _, ok := touchedRoots["websockets"]; ok {
		syncAuthFileWebsocketsAttribute(auth)
	}
	if _, ok := touchedRoots["disabled"]; ok {
		syncAuthFileDisabledState(auth)
	}
	if _, ok := touchedRoots["claude_device_id"]; ok {
		// Fork anti-corr: mirror the edited synthetic Claude device_id into the
		// auth attribute so downstream cloaking reads the operator override.
		syncAuthFileClaudeDeviceIDAttribute(auth)
	}
}

func syncAuthFileHeaderAttributes(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	for key := range auth.Attributes {
		if strings.HasPrefix(key, "header:") {
			delete(auth.Attributes, key)
		}
	}
	for name, value := range coreauth.ExtractCustomHeadersFromMetadata(auth.Metadata) {
		auth.Attributes["header:"+name] = value
	}
}

func syncAuthFilePriorityAttribute(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	priority, ok := authFileIntValue(auth.Metadata["priority"])
	if !ok {
		delete(auth.Attributes, "priority")
		return
	}
	if priority == 0 {
		delete(auth.Attributes, "priority")
		return
	}
	auth.Attributes["priority"] = strconv.Itoa(priority)
}

func authFileIntValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		if i, err := typed.Int64(); err == nil {
			return int(i), true
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return i, true
		}
	}
	return 0, false
}

func syncAuthFileNoteAttribute(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	note, ok := auth.Metadata["note"].(string)
	if !ok {
		delete(auth.Attributes, "note")
		return
	}
	note = strings.TrimSpace(note)
	if note == "" {
		delete(auth.Attributes, "note")
		return
	}
	auth.Attributes["note"] = note
}

func syncAuthFileWebsocketsAttribute(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	websockets, ok := authFileBoolValue(auth.Metadata["websockets"])
	if !ok {
		delete(auth.Attributes, "websockets")
		return
	}
	auth.Attributes["websockets"] = strconv.FormatBool(websockets)
}

func authFileBoolValue(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(typed))
		if errParse == nil {
			return parsed, true
		}
	}
	return false, false
}

func syncAuthFileDisabledState(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	disabled, ok := authFileBoolValue(auth.Metadata["disabled"])
	if !ok {
		return
	}
	auth.Disabled = disabled
	if disabled {
		auth.Status = coreauth.StatusDisabled
		if strings.TrimSpace(auth.StatusMessage) == "" {
			auth.StatusMessage = "disabled via management API"
		}
		return
	}
	auth.Status = coreauth.StatusActive
	auth.StatusMessage = ""
}

func (h *Handler) removeAuth(ctx context.Context, id string) {
	if h == nil || h.authManager == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	if _, ok := h.authManager.GetByID(id); ok {
		h.authManager.Remove(ctx, id)
		return
	}
	authID := h.authIDForPath(id)
	if authID == "" {
		return
	}
	h.authManager.Remove(ctx, authID)
}

func (h *Handler) removeAuthsForPath(ctx context.Context, path string, fallbackID string) {
	if h == nil || h.authManager == nil {
		return
	}
	removed := false
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		if sameAuthFilePath(authAttribute(auth, "path"), path) || sameAuthFilePath(authAttribute(auth, coreauth.AttributeVirtualSource), path) {
			h.removeAuth(ctx, auth.ID)
			removed = true
		}
	}
	if removed {
		return
	}
	if strings.TrimSpace(fallbackID) != "" {
		h.removeAuth(ctx, fallbackID)
		return
	}
	h.removeAuth(ctx, path)
}

func sameAuthFilePath(left, right string) bool {
	left = cleanAuthFilePath(left)
	right = cleanAuthFilePath(right)
	if left == "" || right == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func cleanAuthFilePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if abs, errAbs := filepath.Abs(path); errAbs == nil && strings.TrimSpace(abs) != "" {
		path = abs
	}
	return filepath.Clean(path)
}

func (h *Handler) deleteTokenRecord(ctx context.Context, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("auth path is empty")
	}
	store := h.tokenStoreWithBaseDir()
	if store == nil {
		return fmt.Errorf("token store unavailable")
	}
	return store.Delete(ctx, path)
}

func (h *Handler) tokenStoreWithBaseDir() coreauth.Store {
	if h == nil {
		return nil
	}
	store := h.tokenStore
	if store == nil {
		store = sdkAuth.GetTokenStore()
		h.tokenStore = store
	}
	if h.cfg != nil {
		if dirSetter, ok := store.(interface{ SetBaseDir(string) }); ok {
			dirSetter.SetBaseDir(h.cfg.AuthDir)
		}
	}
	return store
}

func (h *Handler) saveTokenRecord(ctx context.Context, record *coreauth.Auth) (string, error) {
	if record == nil {
		return "", fmt.Errorf("token record is nil")
	}
	store := h.tokenStoreWithBaseDir()
	if store == nil {
		return "", fmt.Errorf("token store unavailable")
	}

	// Bug fix: re-auth flows previously overwrote user-defined fields such as
	// proxy_url / note / headers / refresh_disabled / refresh_enabled / websockets /
	// account_settings. Look up an existing record either by the new ID or by the
	// same provider+account identity so we can merge those fields back in before
	// persisting. When the look-up finds a renamed orphan (e.g. plan-type changed
	// from "plus" to "pro" so the credential filename changed), capture its path
	// so we can clean it up after the new file is saved.
	previous, orphanPath := h.lookupExistingAuthForReauth(record)
	if previous != nil {
		mergeUserDefinedAuthMetadataInto(record, previous)
		inheritUserDefinedAuthAttributesInto(record, previous)
	}

	// Bug fix: a successful reauth / OAuth callback must clear any automatic
	// reauth-required lock it is about to persist so recovery is one step
	// instead of two (reauth + manually re-enabling refresh in the management
	// UI). See clearStaleReauthLockOnSave for why the lock survives the merge
	// above without this.
	clearStaleReauthLockOnSave(record)

	// T3 (telemetry-farm-ux-hardening): a completed re-auth / OAuth callback
	// is the account's designated recovery path out of core's automatic
	// terminal-auth quarantine (AutoQuarantined; see markAutoQuarantine /
	// clearAutoQuarantine in sdk/cliproxy/auth/conductor.go). Once
	// quarantined, the selector skips the credential entirely, so it can
	// never accumulate a fresh "real successful request" to lift the lock on
	// its own -- this call is what actually breaks that deadlock. Some
	// provider-specific record builders (e.g. buildClaudeOAuthTokenRecord)
	// deliberately copy the previous Status/StatusMessage forward for other
	// reasons, so clear unconditionally here rather than relying on every
	// builder to omit AutoQuarantined.
	record.ClearAutoQuarantine()

	if h.postAuthHook != nil {
		if err := h.postAuthHook(ctx, record); err != nil {
			return "", fmt.Errorf("post-auth hook failed: %w", err)
		}
	}
	savedPath, errSave := store.Save(ctx, record)
	if errSave != nil {
		return savedPath, errSave
	}
	if h.postAuthPersistHook != nil {
		if errHook := h.postAuthPersistHook(ctx, record); errHook != nil {
			return savedPath, fmt.Errorf("post-auth persist hook failed: %w", errHook)
		}
	}

	// Remove the stale credential file when the filename changed during re-auth
	// (for example codex plan-type plus->pro). Avoid deleting the file we just
	// wrote.
	if orphanPath != "" && savedPath != "" && filepath.Clean(orphanPath) != filepath.Clean(savedPath) {
		if errRemove := os.Remove(orphanPath); errRemove != nil && !os.IsNotExist(errRemove) {
			log.WithError(errRemove).WithFields(log.Fields{
				"auth_id":       record.ID,
				"provider":      record.Provider,
				"orphan_path":   orphanPath,
				"replaced_with": savedPath,
			}).Warn("re-auth: failed to remove orphan credential file after rename")
		} else if errRemove == nil {
			log.WithFields(log.Fields{
				"auth_id":       record.ID,
				"provider":      record.Provider,
				"orphan_path":   orphanPath,
				"replaced_with": savedPath,
			}).Info("re-auth: removed orphan credential file after filename change")
		}
		// Best effort: ask the auth manager to drop the in-memory entry that
		// pointed at the old filename so the disabled clone doesn't continue to
		// haunt operators.
		if h.authManager != nil && previous != nil && previous.ID != record.ID {
			disabled := previous.Clone()
			disabled.Disabled = true
			disabled.Status = coreauth.StatusDisabled
			disabled.StatusMessage = "superseded by re-auth"
			disabled.UpdatedAt = time.Now()
			if _, errUpdate := h.authManager.Update(ctx, disabled); errUpdate != nil {
				log.WithError(errUpdate).WithFields(log.Fields{
					"auth_id":  previous.ID,
					"provider": previous.Provider,
				}).Debug("re-auth: failed to mark old auth entry disabled after rename")
			}
		}
	}
	return savedPath, nil
}
