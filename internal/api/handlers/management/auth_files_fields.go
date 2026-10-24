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
	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/credentialweight"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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
		c.JSON(clienterror.HTTPStatusFromErrorOr(err, http.StatusBadRequest), gin.H{"error": "invalid request body"})
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

	h.authStatusMu.Lock()
	locked := true
	defer func() {
		if locked {
			h.authStatusMu.Unlock()
		}
	}()

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
		hookAuths, errPatch := h.patchPluginVirtualSourceStatus(ctx, targetAuth, *req.Disabled)
		if errPatch != nil {
			status := http.StatusInternalServerError
			if errors.Is(errPatch, errAuthFileNotFound) || os.IsNotExist(errPatch) {
				status = http.StatusNotFound
			}
			c.JSON(status, gin.H{"error": errPatch.Error()})
			return
		}
		locked = false
		h.authStatusMu.Unlock()
		if errHook := h.invokePostAuthPersistHooks(ctx, hookAuths); errHook != nil {
			log.Errorf("post-auth persist hook failed for plugin virtual source status update on %s: %v", targetAuth.ID, errHook)
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to synchronize plugin virtual auth: %v", errHook)})
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

	updatedAuth, committed, errModify := h.authManager.Modify(ctx, targetAuth.ID, targetAuth.RegistrationEpoch, func(current *coreauth.Auth) error {
		applyAuthDisabledState(current, *req.Disabled)
		return nil
	})
	if errModify != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", errModify)})
		return
	}
	if !committed || updatedAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	locked = false
	h.authStatusMu.Unlock()
	if errHook := h.invokePostAuthPersistHooks(ctx, []*coreauth.Auth{updatedAuth}); errHook != nil {
		log.Errorf("post-auth persist hook failed for status update on %s: %v", targetAuth.ID, errHook)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to synchronize auth runtime: %v", errHook)})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "disabled": *req.Disabled})
}

// patchPluginVirtualSourceStatus toggles disabled on a plugin multi-auth source file and all
// runtime auths expanded from it. Virtual project children cannot be toggled independently.
func (h *Handler) patchPluginVirtualSourceStatus(ctx context.Context, targetAuth *coreauth.Auth, disabled bool) ([]*coreauth.Auth, error) {
	if h == nil || h.authManager == nil || targetAuth == nil {
		return nil, fmt.Errorf("core auth manager unavailable")
	}
	sourcePath := strings.TrimSpace(authAttribute(targetAuth, coreauth.AttributeVirtualSource))
	if sourcePath == "" {
		sourcePath = strings.TrimSpace(authAttribute(targetAuth, "path"))
	}
	if sourcePath == "" {
		return nil, errPluginVirtualAuth
	}
	if errWrite := setSourceAuthFileDisabled(sourcePath, disabled); errWrite != nil {
		if os.IsNotExist(errWrite) {
			return nil, errAuthFileNotFound
		}
		return nil, fmt.Errorf("failed to update source auth file: %w", errWrite)
	}
	now := time.Now()
	hookAuths := make([]*coreauth.Auth, 0)
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		if !sameAuthFilePath(authAttribute(auth, "path"), sourcePath) &&
			!sameAuthFilePath(authAttribute(auth, coreauth.AttributeVirtualSource), sourcePath) {
			continue
		}
		updated, committed, errUpdate := h.authManager.Modify(ctx, auth.ID, auth.RegistrationEpoch, func(current *coreauth.Auth) error {
			applyAuthDisabledState(current, disabled)
			current.UpdatedAt = now
			return nil
		})
		if errUpdate != nil {
			return nil, fmt.Errorf("failed to update auth %s: %w", auth.ID, errUpdate)
		}
		if !committed || updated == nil {
			continue
		}
		hookAuths = append(hookAuths, updated)
	}
	return hookAuths, nil
}

func (h *Handler) invokePostAuthPersistHooks(ctx context.Context, auths []*coreauth.Auth) error {
	if h == nil || h.postAuthPersistHook == nil {
		return nil
	}
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if errHook := h.postAuthPersistHook(ctx, auth); errHook != nil {
			return errHook
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
	coreauth.NormalizeCredentialMetadata(metadata)
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
	// Bug fix: capture the pre-mutation Disabled value so the entire "not
	// disabled" recovery reset below -- Status/StatusMessage/Unavailable,
	// in-memory cooldown/quota/ModelStates, and the auto-quarantine clear
	// (see the "T3" comment further down) -- is gated as one unit on a
	// genuine disabled=true -> disabled=false transition, instead of firing
	// on every call that merely resends disabled=false.
	//
	// Bug fix (regression): a prior version of this function only gated
	// ClearAutoQuarantine() on wasDisabled but still reset
	// Status/StatusMessage/Unavailable/cooldown/quota/ModelStates
	// unconditionally. Auto-quarantine never sets Disabled=true (it is
	// tracked independently via AutoQuarantined/Status), so an
	// already-enabled, auto-quarantined account has wasDisabled=false both
	// before and after any disabled=false save -- e.g. while the operator is
	// only editing an unrelated field via a different endpoint that also
	// happens to resend disabled=false. That no-op resend used to still
	// unconditionally stomp Status back to StatusActive and Unavailable back
	// to false while leaving AutoQuarantined/QuarantineReason/QuarantinedAt
	// untouched (ClearAutoQuarantine itself was correctly skipped) -- a
	// self-contradictory persisted state (auto_quarantined=true but
	// status=active, unavailable=false) that every reader relying on
	// Status/Unavailable instead of AutoQuarantined (e.g. dashboards, health
	// checks, other management endpoints) would misreport as healthy, even
	// though the selector (isAuthBlockedForModel) still correctly blocks the
	// credential on AutoQuarantined alone. Gating the whole reset block on
	// wasDisabled keeps the persisted state internally consistent: a no-op
	// resend now leaves everything untouched, exactly like ClearAutoQuarantine
	// already did.
	wasDisabled := auth.Disabled
	auth.Disabled = disabled
	if disabled {
		auth.Status = coreauth.StatusDisabled
		auth.StatusMessage = "disabled via management API"
	} else if wasDisabled {
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
	// else (disabled == false && !wasDisabled): a no-op resend of the
	// already-"not disabled" state -- there is no genuine transition here, so
	// there is no recovery evidence to act on. Leave Status/StatusMessage/
	// Unavailable/cooldown/quota/ModelStates/AutoQuarantine exactly as they
	// were (see the bug-fix comment above).
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
		c.JSON(clienterror.HTTPStatusFromErrorOr(err, http.StatusBadRequest), gin.H{"error": "invalid request body"})
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
	var errNormalize error
	req, errNormalize = normalizeAuthFilePatchFields(req)
	if errNormalize != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errNormalize.Error()})
		return
	}
	requestRetryPatch, errRequestRetry := decodeAuthFileRequestRetryPatch(req)
	if errRequestRetry != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errRequestRetry.Error()})
		return
	}
	for key := range req {
		if strings.TrimSpace(key) == "request_retry" {
			delete(req, key)
		}
	}

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

	// Config-sourced credentials are managed through config endpoints, not as auth files.
	if targetAuth == nil || targetAuth.AuthSourceKind() == coreauth.AuthSourceConfig {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if coreauth.IsPluginVirtualAuth(targetAuth) {
		c.JSON(http.StatusConflict, gin.H{"error": errPluginVirtualAuth.Error()})
		return
	}

	updatedAuth, committed, errModify := h.authManager.Modify(ctx, targetAuth.ID, targetAuth.RegistrationEpoch, func(current *coreauth.Auth) error {
		return applyAuthFileFieldsPatch(current, req, requestRetryPatch)
	})
	if errModify != nil {
		var patchErr *authFileFieldPatchError
		if errors.As(errModify, &patchErr) {
			c.JSON(http.StatusBadRequest, gin.H{"error": patchErr.message})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", errModify)})
		return
	}
	if !committed || updatedAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if h.postAuthPersistHook != nil {
		if errHook := h.postAuthPersistHook(ctx, updatedAuth); errHook != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("post-auth persist hook failed: %v", errHook)})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

type authFileFieldPatchError struct {
	message string
}

func (e *authFileFieldPatchError) Error() string {
	return e.message
}

func applyAuthFileFieldsPatch(targetAuth *coreauth.Auth, req map[string]json.RawMessage, requestRetryPatch authFileRequestRetryPatch) error {
	coreauth.NormalizeCredentialMetadata(targetAuth.Metadata)

	changed := false
	touchedRoots := make(map[string]struct{}, len(req))
	for key, rawValue := range req {
		fieldPath := strings.TrimSpace(key)
		if fieldPath == "" {
			return &authFileFieldPatchError{message: "field name is required"}
		}
		value, errDecode := decodeAuthFileFieldValue(rawValue)
		if errDecode != nil {
			return &authFileFieldPatchError{message: fmt.Sprintf("invalid field %s", fieldPath)}
		}
		if rootAuthFileField(fieldPath) == "excluded_models" {
			if fieldPath != "excluded_models" {
				return &authFileFieldPatchError{message: "excluded_models does not support nested fields"}
			}
			if value != nil {
				models, okModels := value.([]any)
				validModels := okModels
				for _, model := range models {
					if _, okString := model.(string); !okString {
						validModels = false
						break
					}
				}
				if !validModels {
					return &authFileFieldPatchError{message: "excluded_models must be a string array or null"}
				}
			}
		}
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}

		if fieldPath == coreauth.AttributeWeight {
			if value == nil {
				delete(targetAuth.Metadata, coreauth.AttributeWeight)
			} else {
				if _, okNumber := value.(json.Number); !okNumber {
					return &authFileFieldPatchError{message: "weight must be an integer"}
				}
				weight, errWeight := credentialweight.ParseValue(value)
				if errWeight != nil {
					return &authFileFieldPatchError{message: errWeight.Error()}
				}
				targetAuth.Metadata[coreauth.AttributeWeight] = weight
			}
		} else if rootAuthFileField(fieldPath) == coreauth.AttributeWeight {
			return &authFileFieldPatchError{message: "weight does not support nested fields"}
		} else if fieldPath == "headers" {
			applyAuthFileHeadersPatch(targetAuth, value)
		} else if fieldPath == "proxy_url" {
			// Fork anti-corr: a present-but-invalid proxy_url must never be
			// persisted. An invalid account proxy forces a direct egress that
			// exposes the real server IP. Empty is allowed (clears the override).
			if errValidate := validateAuthFileProxyURLPatch(value); errValidate != nil {
				return &authFileFieldPatchError{message: errValidate.Error()}
			}
			if errSet := setAuthFileMetadataValue(targetAuth.Metadata, fieldPath, value); errSet != nil {
				return &authFileFieldPatchError{message: errSet.Error()}
			}
		} else if fieldPath == "claude_device_id" {
			// Fork anti-corr: the per-account synthetic Claude device_id is
			// operator-editable but must stay a well-formed UUID so it keeps
			// masking the real machine device_id across accounts.
			if errValidate := validateAuthFileClaudeDeviceIDPatch(value); errValidate != nil {
				return &authFileFieldPatchError{message: errValidate.Error()}
			}
			if errSet := setAuthFileMetadataValue(targetAuth.Metadata, fieldPath, value); errSet != nil {
				return &authFileFieldPatchError{message: errSet.Error()}
			}
		} else if fieldPath == "farm_container_alive_at" {
			// Fork farm container-liveness gate: the heartbeat written by the
			// orchestrator must be a well-formed RFC3339 timestamp (or an empty
			// string to clear it) so a malformed value can never be persisted and
			// later mis-read as a fresh heartbeat by the gate.
			if errValidate := validateFarmAliveAtPatch(value); errValidate != nil {
				return &authFileFieldPatchError{message: errValidate.Error()}
			}
			if errSet := setAuthFileMetadataValue(targetAuth.Metadata, fieldPath, value); errSet != nil {
				return &authFileFieldPatchError{message: errSet.Error()}
			}
		} else if errSet := setAuthFileMetadataValue(targetAuth.Metadata, fieldPath, value); errSet != nil {
			return &authFileFieldPatchError{message: errSet.Error()}
		}
		if root := rootAuthFileField(fieldPath); root != "" {
			touchedRoots[root] = struct{}{}
		}
		changed = true
	}
	if requestRetryPatch.Set {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		if requestRetryPatch.Value == nil {
			delete(targetAuth.Metadata, "request_retry")
		} else {
			targetAuth.Metadata["request_retry"] = *requestRetryPatch.Value
		}
		changed = true
	}
	if changed {
		syncAuthFileMetadataFields(targetAuth, touchedRoots)
		if _, touched := touchedRoots["excluded_models"]; touched {
			excluded, errExcluded := synthesizer.ExtractExcludedModelsFromMetadata(targetAuth.Metadata)
			if errExcluded != nil {
				return &authFileFieldPatchError{message: errExcluded.Error()}
			}
			// ApplyAuthExcludedModelsMeta never clears attributes, so drop stale values first.
			delete(targetAuth.Attributes, "excluded_models")
			delete(targetAuth.Attributes, "excluded_models_hash")
			// Store only the credential's own rules; global rules are resolved at model registration.
			// An empty authKind keeps the existing auth_kind attribute unchanged.
			synthesizer.ApplyAuthExcludedModelsMeta(targetAuth, excluded, "")
		}
	}

	if !changed {
		return &authFileFieldPatchError{message: "no fields to update"}
	}

	targetAuth.UpdatedAt = time.Now()
	return nil
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

type authFileRequestRetryPatch struct {
	Set   bool
	Value *int
}

func normalizeAuthFilePatchFields(fields map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	normalized := make(map[string]json.RawMessage, len(fields))
	originalNames := make(map[string]string, len(fields))
	canonicalNames := make(map[string]bool, len(fields))
	for key, value := range fields {
		parts := strings.Split(strings.TrimSpace(key), ".")
		for index := range parts {
			parts[index] = strings.TrimSpace(parts[index])
		}
		originalRoot := parts[0]
		parts[0] = coreauth.CanonicalCredentialMetadataKey(originalRoot)
		canonicalPath := strings.Join(parts, ".")
		if original, exists := originalNames[canonicalPath]; exists {
			currentCanonical := originalRoot == parts[0]
			if canonicalNames[canonicalPath] != currentCanonical {
				if currentCanonical {
					normalized[canonicalPath] = value
					originalNames[canonicalPath] = key
					canonicalNames[canonicalPath] = true
				}
				continue
			}
			return nil, fmt.Errorf("auth file fields %q and %q refer to the same field", original, key)
		}
		normalized[canonicalPath] = value
		originalNames[canonicalPath] = key
		canonicalNames[canonicalPath] = originalRoot == parts[0]
	}
	return normalized, nil
}

func decodeAuthFileRequestRetryPatch(fields map[string]json.RawMessage) (authFileRequestRetryPatch, error) {
	var raw json.RawMessage
	found := false
	for key, value := range fields {
		fieldPath := strings.TrimSpace(key)
		fieldRoot := rootAuthFileField(fieldPath)
		if fieldRoot == "request_retry" && fieldPath != fieldRoot {
			return authFileRequestRetryPatch{}, fmt.Errorf("request_retry does not support nested fields")
		}
		if fieldPath == "request_retry" {
			found = true
			raw = value
		}
	}
	if !found {
		return authFileRequestRetryPatch{}, nil
	}
	value, errDecode := decodeAuthFileFieldValue(raw)
	if errDecode != nil {
		return authFileRequestRetryPatch{}, fmt.Errorf("request_retry must be an integer or null")
	}
	if value == nil {
		return authFileRequestRetryPatch{Set: true}, nil
	}
	number, okNumber := value.(json.Number)
	if !okNumber {
		return authFileRequestRetryPatch{}, fmt.Errorf("request_retry must be an integer or null")
	}
	parsed, errInt := number.Int64()
	if errInt != nil {
		return authFileRequestRetryPatch{}, fmt.Errorf("request_retry must be an integer or null")
	}
	normalized := int(parsed)
	if int64(normalized) != parsed {
		return authFileRequestRetryPatch{}, fmt.Errorf("request_retry must be an integer or null")
	}
	if normalized < 0 {
		return authFileRequestRetryPatch{Set: true}, nil
	}
	return authFileRequestRetryPatch{Set: true, Value: &normalized}, nil
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
	if _, ok := touchedRoots[coreauth.AttributeWeight]; ok {
		syncAuthFileWeightAttribute(auth)
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
	if _, ok := touchedRoots["farm_container_alive_at"]; ok {
		// Fork farm container-liveness gate: mirror the just-patched heartbeat
		// timestamp into the live Attributes map immediately so the gate
		// (coreauth.authContainerRecentlyAlive, which reads Attributes) observes
		// it in-process without waiting for a full store reload. Without this the
		// PATCHed value would live only in Metadata and the gate would never see
		// it.
		syncAuthFileContainerAliveAtAttribute(auth)
	}
	if _, ok := touchedRoots["plan_type"]; ok {
		syncAuthFilePlanTypeAttribute(auth)
	} else if _, ok := touchedRoots["id_token"]; ok {
		syncAuthFilePlanTypeAttribute(auth)
	}
}

func syncAuthFilePlanTypeAttribute(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	newPlanType := ""
	if auth.Metadata != nil {
		if ptRaw, ok := auth.Metadata["plan_type"].(string); ok && strings.TrimSpace(ptRaw) != "" {
			newPlanType = strings.TrimSpace(ptRaw)
		} else if idTokenRaw, ok := auth.Metadata["id_token"].(string); ok && strings.TrimSpace(idTokenRaw) != "" {
			if claims, errParse := codex.ParseJWTToken(idTokenRaw); errParse == nil && claims != nil {
				newPlanType = claims.GetPlanType()
			} else {
				newPlanType = codex.DefaultPlanType
			}
		}
	}
	if newPlanType != "" {
		auth.Attributes["plan_type"] = newPlanType
	} else {
		delete(auth.Attributes, "plan_type")
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
		delete(auth.Attributes, coreauth.AttributeFilePriority)
		return
	}
	if auth.Attributes[coreauth.AttributeSourceBackend] == coreauth.AuthSourceFile {
		auth.Attributes[coreauth.AttributeFilePriority] = "true"
	}
	if priority == 0 {
		delete(auth.Attributes, "priority")
		return
	}
	auth.Attributes["priority"] = strconv.Itoa(priority)
}

func syncAuthFileWeightAttribute(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	weight, errWeight := credentialweight.ParseValue(auth.Metadata[coreauth.AttributeWeight])
	if errWeight != nil {
		delete(auth.Attributes, coreauth.AttributeWeight)
		return
	}
	auth.Attributes[coreauth.AttributeWeight] = strconv.FormatInt(weight, 10)
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
	// Bug fix: same class of status-drift as applyAuthDisabledState. Capture
	// the pre-mutation Disabled value so the "not disabled" Status/
	// StatusMessage reset below only fires on a genuine disabled=true ->
	// disabled=false transition, not on every PatchAuthFileFields call that
	// merely resends disabled=false (e.g. while editing an unrelated field).
	//
	// Auto-quarantine (see conductor_auto_quarantine.go) never sets
	// Disabled=true -- it is tracked independently via AutoQuarantined/
	// Status=StatusQuarantined -- so an already-enabled, auto-quarantined
	// account has wasDisabled=false both before and after any disabled=false
	// save here. Without this gate, that no-op resend would unconditionally
	// stomp Status back to StatusActive (while AutoQuarantined stays true),
	// producing the same self-contradictory persisted state
	// (auto_quarantined=true but status=active) that applyAuthDisabledState
	// was fixed to avoid.
	wasDisabled := auth.Disabled
	auth.Disabled = disabled
	if disabled {
		auth.Status = coreauth.StatusDisabled
		if strings.TrimSpace(auth.StatusMessage) == "" {
			auth.StatusMessage = "disabled via management API"
		}
		return
	}
	if !wasDisabled {
		// No-op resend of the already-"not disabled" state: no genuine
		// transition, so leave Status/StatusMessage untouched.
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
	if cfg := h.configSnapshot(); cfg != nil {
		if dirSetter, ok := store.(interface{ SetBaseDir(string) }); ok {
			dirSetter.SetBaseDir(cfg.AuthDir)
		}
	}
	return store
}

func (h *Handler) mergeExistingAuthFileMetadata(record *coreauth.Auth) map[string]any {
	if h == nil || record == nil {
		return nil
	}
	var existingMap map[string]any

	if cfg := h.configSnapshot(); cfg != nil && strings.TrimSpace(cfg.AuthDir) != "" {
		targetFile := record.FileName
		if targetFile == "" {
			targetFile = record.ID
		}
		if targetFile != "" {
			fullPath := filepath.Join(cfg.AuthDir, targetFile)
			if raw, errRead := os.ReadFile(fullPath); errRead == nil && len(raw) > 0 {
				_ = json.Unmarshal(raw, &existingMap)
			}
		}
	}

	if existingMap == nil && h.authManager != nil {
		if existing, ok := h.authManager.GetByID(record.ID); ok && existing != nil && existing.Metadata != nil {
			existingMap = existing.Metadata
		} else {
			for _, auth := range h.authManager.List() {
				if auth != nil && auth.FileName == record.FileName && auth.Metadata != nil {
					existingMap = auth.Metadata
					break
				}
			}
		}
	}

	if len(existingMap) > 0 {
		coreauth.MergeExistingAuthMetadata(record, existingMap)
	}
	return existingMap
}

func (h *Handler) saveTokenRecord(ctx context.Context, record *coreauth.Auth) (string, error) {
	if record == nil {
		return "", fmt.Errorf("token record is nil")
	}
	if h.authManager != nil {
		if existing, ok := h.authManager.GetByID(record.ID); ok && !coreauth.ReauthenticationIdentityMatches(existing, record) {
			return "", fmt.Errorf("reauthenticate auth %s: account identity changed", record.ID)
		}
	}
	// 只保留调用者本次明确给出的quota结果，区分后续通用合并补入的旧观测。
	incomingQuotaRuntime := make(map[string]any)
	for key, value := range record.Metadata {
		if isReauthRuntimeMetadataKey(key) {
			incomingQuotaRuntime[key] = value
		}
	}
	existingMetadata := h.mergeExistingAuthFileMetadata(record)
	store := h.tokenStoreWithBaseDir()
	if store == nil {
		return "", fmt.Errorf("token store unavailable")
	}
	legacyClaudeCredential, errLegacy := claudeauth.FindMatchingLegacyCredential(ctx, store, record)
	if errLegacy != nil {
		return "", errLegacy
	}
	if legacyClaudeCredential != nil {
		coreauth.MergeExistingAuthMetadata(record, legacyClaudeCredential.Metadata)
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
	} else {
		// TR4 (telemetry-farm-ux-hardening): previous == nil means
		// lookupExistingAuthForReauth found no prior record for this account
		// (neither by ID nor by provider+email/account_id), i.e. this save is
		// the account's first completed authentication. Auto-enroll it into
		// the device farm at creation time so operators do not have to
		// remember to flip the enrollment toggle by hand for every new
		// account. Deliberately keyed off previous == nil rather than
		// record.CreatedAt / Auth file mtime, since those get rewritten by
		// later reauth and would misclassify an existing account as "new".
		// Existing accounts going through reauth take the previous != nil
		// branch above instead, and their current farm_enrolled value is
		// already carried forward untouched by mergeUserDefinedAuthMetadataInto
		// (the key is not in reauthTokenMetadataKeys/reauthRuntimeMetadataKeys),
		// so this branch never re-enrolls or un-enrolls them.
		//
		// H4 (telemetry-farm): auto-enrollment is now gated behind the global
		// farm-auto-enroll switch (config.FarmAutoEnrollEnabled, default true).
		// When an operator flips it off, first-auth accounts are left unenrolled
		// (farm_enrolled key absent => coreauth.AuthFarmEnrolled false default)
		// so enrollment becomes strictly manual opt-in via the account-level
		// toggle. h.cfg is the live in-memory config the management handler
		// mutates on PUT and that hot-reload swaps via SetConfig, so this read
		// always observes the current switch value.
		if config.FarmAutoEnrollEnabled(h.configSnapshot()) {
			applyAuthFarmEnrolledMetadata(record, true)
		}
	}

	// Bug fix: a successful reauth / OAuth callback must clear any automatic
	// reauth-required lock it is about to persist so recovery is one step
	// instead of two (reauth + manually re-enabling refresh in the management
	// UI). See clearStaleReauthLockOnSave for why the lock survives the merge
	// above without this.
	recoveryBase := existingMetadata
	if previous != nil {
		recoveryBase = previous.Metadata
	}
	canClearRecovery := !coreauth.IsCodexAuthRecord(record) || coreauth.LoginReplacesOAuthCredentials(record.Metadata, recoveryBase)
	if canClearRecovery {
		// 通用metadata合并可能已带回旧quota错误；仅完整恢复边界清理，
		// Codex单分区更新继续保留旧观测/硬锁，operator设置不在此集合中。
		for key := range record.Metadata {
			if isReauthRuntimeMetadataKey(key) {
				delete(record.Metadata, key)
			}
		}
		for key, value := range incomingQuotaRuntime {
			record.Metadata[key] = value
		}
		clearStaleReauthLockOnSave(record)
	}

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
	if canClearRecovery {
		record.ClearAutoQuarantine()
	} else if previous != nil && previous.AutoQuarantined {
		// An outer-only reauthentication cannot release a dead SIWC partition.
		record.AutoQuarantined = previous.AutoQuarantined
		record.QuarantineReason = previous.QuarantineReason
		record.QuarantinedAt = previous.QuarantinedAt
		record.Status, record.StatusMessage = previous.Status, previous.StatusMessage
		record.Unavailable = previous.Unavailable
		coreauth.MergeExistingAuthMetadata(record, previous.Metadata)
	}

	if h.postAuthHook != nil {
		if err := h.postAuthHook(ctx, record); err != nil {
			return "", fmt.Errorf("post-auth hook failed: %w", err)
		}
	}
	savedPath, errSave := store.Save(coreauth.WithAuthCreationIntent(ctx), record)
	if errSave != nil {
		return savedPath, errSave
	}
	if legacyClaudeCredential != nil {
		if strings.TrimSpace(savedPath) == "" {
			return "", fmt.Errorf("canonical Claude credential was not persisted; legacy credential retained")
		}
		legacyID := strings.TrimSpace(legacyClaudeCredential.ID)
		if legacyID == "" {
			legacyID = strings.TrimSpace(legacyClaudeCredential.FileName)
		}
		if errDelete := store.Delete(ctx, legacyID); errDelete != nil {
			return savedPath, fmt.Errorf("canonical Claude credential saved but legacy credential cleanup failed: %w", errDelete)
		}
	}
	// Only a completed credential replacement may open a new registration. A
	// partial Codex/SIWC replacement keeps the existing epoch and terminal lock.
	reauthenticated := canClearRecovery && previous != nil && previous.ID == record.ID && coreauth.CredentialsChanged(previous, record)
	if h.postAuthPersistHook != nil || (reauthenticated && h.authManager != nil) {
		persistedRecord := record
		if data, errRead := os.ReadFile(savedPath); errRead == nil && len(data) > 0 {
			auths, errSynthesize := synthesizer.SynthesizeAuthFile(&synthesizer.SynthesisContext{
				Config:           h.configSnapshot(),
				AuthDir:          filepath.Dir(savedPath),
				Now:              time.Now(),
				IDGenerator:      synthesizer.NewStableIDGenerator(),
				PluginAuthParser: h.pluginHost,
			}, savedPath, data)
			if errSynthesize != nil {
				return savedPath, fmt.Errorf("synthesize persisted auth failed: %w", errSynthesize)
			}
			for _, auth := range auths {
				if auth != nil && (auth.ID == record.ID || len(auths) == 1) {
					persistedRecord = auth
					break
				}
			}
		}
		// File synthesis restores storage fields; retain runtime-only operator
		// attributes already merged above without overriding synthesized identity.
		inheritUserDefinedAuthAttributesInto(persistedRecord, record)
		if reauthenticated && h.authManager != nil {
			// FileTokenStore has already committed. Registration must not save again or
			// turn a failed save into an in-memory success. The subsequent runtime hook
			// receives the new epoch so it cannot re-adopt the retired registration.
			registered, errRegister := h.authManager.RegisterReauthenticated(coreauth.WithSkipPersist(ctx), persistedRecord)
			if errRegister != nil {
				return savedPath, fmt.Errorf("register reauthenticated credential: %w", errRegister)
			}
			if registered != nil {
				persistedRecord = registered
			}
		}
		if h.postAuthPersistHook != nil {
			if errHook := h.postAuthPersistHook(ctx, persistedRecord); errHook != nil {
				return savedPath, fmt.Errorf("post-auth persist hook failed: %w", errHook)
			}
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
