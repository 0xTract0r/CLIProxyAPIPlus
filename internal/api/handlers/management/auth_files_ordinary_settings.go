package management

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type ordinaryAccountSettingsView struct {
	Note           string   `json:"note"`
	RefreshEnabled bool     `json:"refresh_enabled"`
	Disabled       bool     `json:"disabled"`
	AuthIndex      string   `json:"auth_index"`
	Revision       string   `json:"revision,omitempty"`
	EditableFields []string `json:"editable_fields"`
	Warnings       []string `json:"warnings,omitempty"`
}

type ordinaryAccountSettingsRequest struct {
	name, index, revision string
	note                  *string
	disabled              *bool
	refreshEnabled        *bool
}

type ordinaryAccountSettingsTarget struct {
	manager *coreauth.Manager
	auth    *coreauth.Auth
	dir     string
	file    sdkauth.AccountSettingsFile
}

func (h *Handler) GetOrdinaryAuthFileAccountSettings(c *gin.Context) {
	req := ordinaryAccountSettingsRequest{name: strings.TrimSpace(c.Query("name")), index: strings.TrimSpace(c.Query("auth_index"))}
	target, status, message := h.ordinaryAccountSettingsTarget(req)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	writeOrdinaryAccountSettings(c, target.auth, target.revision(), nil)
}

func (h *Handler) PatchOrdinaryAuthFileAccountSettings(c *gin.Context) {
	req, status, err := decodeOrdinaryAccountSettings(c)
	if err != nil {
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	h.authStatusMu.Lock()
	locked := true
	defer func() {
		if locked {
			h.authStatusMu.Unlock()
		}
	}()
	target, status, message := h.ordinaryAccountSettingsTarget(req)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}

	expected := target.revision()
	header := strings.TrimSpace(c.GetHeader("If-Match"))
	if req.refreshEnabled != nil && header == "" && req.revision == "" {
		c.JSON(http.StatusPreconditionRequired, gin.H{"error": "read the credential settings before editing automatic refresh"})
		return
	}
	if (header != "" && header != `"`+expected+`"`) ||
		(req.revision != "" && req.revision != expected) {
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": "credential file changed, reload its settings"})
		return
	}

	ctx := c.Request.Context()
	writeCtx := ctx
	if target.file.Valid() {
		writeCtx = sdkauth.WithAccountSettingsFilePatch(ctx, target.file, req.note != nil, req.disabled != nil, req.refreshEnabled != nil)
	}
	updated, committed, err := target.manager.ModifyPersisted(writeCtx, target.auth.ID, target.auth.RegistrationEpoch, func(current *coreauth.Auth) error {
		h.mu.Lock()
		sameTarget := h.authManager == target.manager && resolvedV8SynthesisAuthDir(h.cfg) == target.dir
		h.mu.Unlock()
		if !sameTarget || authAttribute(current, "path") != authAttribute(target.auth, "path") {
			return coreauth.ErrFileRemovalBusy
		}
		stored := ordinaryAccountSettingsTarget{auth: current}
		if target.file.Valid() {
			currentFile, err := sdkauth.ReadAccountSettingsFile(target.dir, authAttribute(current, "path"))
			if err != nil {
				return err
			}
			stored.file = currentFile
		}
		if stored.revision() != expected {
			return coreauth.ErrFileRemovalBusy
		}
		if current.Metadata == nil {
			current.Metadata = make(map[string]any)
		}
		if req.note != nil {
			current.Metadata["note"] = *req.note
			syncAuthFileNoteAttribute(current)
		}
		if req.refreshEnabled != nil {
			existing, _ := current.Metadata["account_settings"].(map[string]any)
			settings := make(map[string]any, len(existing)+1)
			for key, value := range existing {
				settings[key] = value
			}
			if *req.refreshEnabled && !current.AutoQuarantined && !coreauth.IsReauthRequiredMetadata(current.Metadata) {
				// Previous editors wrote the operator flag both here and at the file root.
				for _, key := range []string{"refresh_disabled", "disable_refresh", "auto_refresh_disabled", "refresh_enabled"} {
					delete(current.Metadata, key)
					delete(current.Attributes, key)
				}
			}
			settings["refresh_enabled"] = *req.refreshEnabled
			current.Metadata["account_settings"] = settings
		}
		if req.disabled != nil {
			current.Metadata["disabled"] = *req.disabled
			syncAuthFileDisabledState(current)
		}
		return nil
	})
	if err != nil {
		if writeAuthFileStorageConflict(c, err) {
			return
		}
		// ModifyPersisted rejects a stale registration epoch before calling edit.
		latest, ok := target.manager.GetByID(target.auth.ID)
		if !ok || latest.RegistrationEpoch != target.auth.RegistrationEpoch {
			c.JSON(http.StatusConflict, gin.H{"error": "credential was replaced, reload its settings"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist ordinary credential settings"})
		return
	}
	if !committed || updated == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "credential was replaced, reload its settings"})
		return
	}
	revision := ordinaryAccountSettingsTarget{auth: updated}.revision()
	if target.file.Valid() {
		revision = ""
		if persisted, err := sdkauth.ReadAccountSettingsFile(target.dir, authAttribute(updated, "path")); err == nil {
			revision = persisted.Revision()
		}
	}

	locked = false
	h.authStatusMu.Unlock()
	var warnings []string
	if err := h.invokePostAuthPersistHooks(ctx, []*coreauth.Auth{updated}); err != nil {
		// The disk and manager have committed. A transport error would invite a duplicate edit.
		warnings = []string{"settings were saved, but runtime synchronization reported an error"}
	}
	writeOrdinaryAccountSettings(c, updated, revision, warnings)
}

func (h *Handler) ordinaryAccountSettingsTarget(req ordinaryAccountSettingsRequest) (ordinaryAccountSettingsTarget, int, string) {
	var result ordinaryAccountSettingsTarget
	if req.name == "" && req.index == "" {
		return result, http.StatusBadRequest, "name or auth_index is required"
	}
	h.mu.Lock()
	result.manager, result.dir = h.authManager, resolvedV8SynthesisAuthDir(h.cfg)
	store := h.tokenStore
	h.mu.Unlock()
	if store == nil {
		store = sdkauth.GetTokenStore()
	}
	if result.manager == nil {
		return result, http.StatusServiceUnavailable, "core auth manager unavailable"
	}

	for _, candidate := range result.manager.List() {
		if !matchesAuthFileLookup(candidate, req.name, req.index) {
			continue
		}
		if result.auth != nil {
			return ordinaryAccountSettingsTarget{}, http.StatusConflict, "ambiguous credential name, supply auth_index"
		}
		result.auth = candidate
	}
	if result.auth == nil {
		return result, http.StatusNotFound, "auth file not found"
	}
	if coreauth.IsConfigAPIKeyAuth(result.auth) || coreauth.IsPluginVirtualAuth(result.auth) || isRuntimeOnlyAuth(result.auth) {
		return result, http.StatusConflict, "this credential has no directly editable source file"
	}
	path := strings.TrimSpace(authAttribute(result.auth, "path"))
	_, isLocal := store.(*sdkauth.FileTokenStore)
	if isLocal || path != "" {
		snapshot, err := sdkauth.ReadAccountSettingsFile(result.dir, path)
		if err != nil {
			if errors.Is(err, coreauth.ErrFileRemovalBusy) {
				return result, http.StatusConflict, "credential file changed, reload its settings"
			}
			return result, http.StatusNotFound, "auth file not found or outside the current directory"
		}
		result.file = snapshot
	}
	return result, 0, ""
}

func (target ordinaryAccountSettingsTarget) revision() string {
	if target.file.Valid() {
		return target.file.Revision()
	}
	state, _ := json.Marshal(struct {
		ID             string
		Epoch          uint64
		Generation     uint64
		Note           string
		Disabled       bool
		RefreshEnabled bool
	}{target.auth.ID, target.auth.RegistrationEpoch, target.auth.Generation, ordinaryAccountNote(target.auth), target.auth.Disabled, ordinaryRefreshEnabled(target.auth)})
	sum := sha256.Sum256(state)
	return hex.EncodeToString(sum[:])
}

func ordinaryAccountNote(auth *coreauth.Auth) string {
	if note := strings.TrimSpace(authAttribute(auth, "note")); note != "" {
		return note
	}
	raw, _ := auth.Metadata["note"].(string)
	return strings.TrimSpace(raw)
}

func ordinaryRefreshEnabled(auth *coreauth.Auth) bool {
	if settings, ok := auth.Metadata["account_settings"].(map[string]any); ok {
		if enabled, ok := settings["refresh_enabled"].(bool); ok {
			return enabled
		}
	}
	return !auth.RefreshDisabled()
}

func writeOrdinaryAccountSettings(c *gin.Context, auth *coreauth.Auth, revision string, warnings []string) {
	if revision != "" {
		c.Header("ETag", `"`+revision+`"`)
	}
	refreshEnabled := ordinaryRefreshEnabled(auth)
	if refreshEnabled && auth.RefreshDisabled() {
		warnings = append(warnings, "automatic refresh is still blocked by credential state or legacy settings")
	}
	c.Header("Cache-Control", "no-store")

	c.JSON(http.StatusOK, gin.H{
		"name": authFileListName(auth),
		"account_settings": ordinaryAccountSettingsView{
			Note: ordinaryAccountNote(auth), Disabled: auth.Disabled, AuthIndex: lockedAuthIndex(auth), Revision: revision,
			RefreshEnabled: refreshEnabled, EditableFields: []string{"note", "disabled", "refresh_enabled"}, Warnings: warnings,
		},
	})
}

func decodeOrdinaryAccountSettings(c *gin.Context) (ordinaryAccountSettingsRequest, int, error) {
	var req ordinaryAccountSettingsRequest
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10))
	open, err := dec.Token()
	if err != nil || open != json.Delim('{') {
		return req, http.StatusBadRequest, errors.New("request body must be a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		key, err := dec.Token()
		name, ok := key.(string)
		if err != nil || !ok {
			return req, http.StatusBadRequest, errors.New("invalid field name")
		}
		if _, exists := fields[name]; exists {
			return req, http.StatusBadRequest, errors.New("duplicate field: " + name)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				return req, http.StatusRequestEntityTooLarge, errors.New("account settings request is too large")
			}
			return req, http.StatusBadRequest, errors.New("invalid value for " + name)
		}
		fields[name] = value
	}
	if _, err := dec.Token(); err != nil {
		return req, http.StatusBadRequest, errors.New("invalid request body")
	}
	if _, err := dec.Token(); err != io.EOF {
		return req, http.StatusBadRequest, errors.New("unexpected data after account settings")
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		raw := fields[key]
		switch key {
		case "name", "auth_index", "revision", "note":
			var text *string
			if err := json.Unmarshal(raw, &text); err != nil || text == nil {
				return req, http.StatusBadRequest, errors.New(key + " must be a string")
			}
			value := strings.TrimSpace(*text)
			switch key {
			case "name":
				req.name = value
			case "auth_index":
				req.index = value
			case "revision":
				req.revision = value
			case "note":
				req.note = &value
			}
		case "refresh_enabled":
			if err := json.Unmarshal(raw, &req.refreshEnabled); err != nil || req.refreshEnabled == nil {
				return req, http.StatusBadRequest, errors.New("refresh_enabled must be a boolean")
			}
		case "disabled":
			if err := json.Unmarshal(raw, &req.disabled); err != nil || req.disabled == nil {
				return req, http.StatusBadRequest, errors.New("disabled must be a boolean")
			}
		default:
			return req, http.StatusBadRequest, errors.New("unsupported account settings field: " + key)
		}
	}
	if req.note == nil && req.disabled == nil && req.refreshEnabled == nil {
		return req, http.StatusBadRequest, errors.New("no editable account settings fields supplied")
	}
	return req, 0, nil
}
