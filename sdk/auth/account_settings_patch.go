package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/copilot"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const maxAccountSettingsFile = 2 << 20

// AccountSettingsFile identifies the observed local file and its exact contents.
type AccountSettingsFile struct {
	dir, path, relative string
	info                os.FileInfo
	digest              [sha256.Size]byte
}

// ReadAccountSettingsFile captures the identity of a regular file scoped to AuthDir.
func ReadAccountSettingsFile(dir, path string) (AccountSettingsFile, error) {
	dir, path = strings.TrimSpace(dir), strings.TrimSpace(path)
	if dir == "" || path == "" {
		return AccountSettingsFile{}, coreauth.ErrCredentialFileRemoved
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return AccountSettingsFile{}, coreauth.ErrCredentialFileRemoved
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return AccountSettingsFile{}, coreauth.ErrCredentialFileRemoved
	}
	rel, err := filepath.Rel(base, absolute)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return AccountSettingsFile{}, coreauth.ErrCredentialFileRemoved
	}
	snapshot := AccountSettingsFile{dir: base, path: absolute, relative: rel}
	data, info, err := snapshot.read()
	if err != nil {
		return AccountSettingsFile{}, err
	}
	snapshot.info, snapshot.digest = info, sha256.Sum256(data)
	return snapshot, nil
}

func (f AccountSettingsFile) Valid() bool { return f.info != nil }

func (f AccountSettingsFile) Revision() string {
	return hex.EncodeToString(f.digest[:])
}

func (f AccountSettingsFile) read() ([]byte, os.FileInfo, error) {
	root, err := os.OpenRoot(f.dir)
	if err != nil {
		return nil, nil, coreauth.ErrCredentialFileRemoved
	}
	defer root.Close()
	expected, err := root.Lstat(f.relative)
	if err != nil || !expected.Mode().IsRegular() {
		return nil, nil, coreauth.ErrCredentialFileRemoved
	}
	file, err := root.Open(f.relative)
	if err != nil {
		return nil, nil, coreauth.ErrCredentialFileRemoved
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(expected, info) {
		return nil, nil, coreauth.ErrFileRemovalBusy
	}
	data, err := io.ReadAll(io.LimitReader(file, maxAccountSettingsFile+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > maxAccountSettingsFile {
		return nil, nil, errors.New("credential file exceeds account settings size limit")
	}
	return data, info, nil
}

// CopilotAccountID reads the positive GitHub ID returned by the subject lookup.
// Older records with only a name, email or filename do not identify a subject.
func CopilotAccountID(value any) (string, bool) {
	var raw string
	switch typed := value.(type) {
	case string:
		raw = strings.TrimSpace(typed)
	case json.Number:
		raw = typed.String()
	default:
		return "", false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 || raw != strconv.FormatInt(id, 10) {
		return "", false
	}
	return raw, true
}

func copilotFileIdentity(data []byte) (string, string, error) {
	var entry struct {
		Type      string `json:"type"`
		AccountID any    `json:"account_id"`
		ProxyURL  string `json:"proxy_url"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if !json.Valid(data) || decoder.Decode(&entry) != nil || !strings.EqualFold(strings.TrimSpace(entry.Type), copilot.Provider) {
		return "", "", errors.New("selected file is not a Copilot credential")
	}
	id, ok := CopilotAccountID(entry.AccountID)
	if !ok {
		return "", "", errors.New("selected Copilot credential has no verified GitHub account ID")
	}
	return id, strings.TrimSpace(entry.ProxyURL), nil
}

// CopilotIdentity returns the subject and route of this exact inspected file.
func (f AccountSettingsFile) CopilotIdentity() (string, string, error) {
	if !f.Valid() {
		return "", "", coreauth.ErrCredentialFileRemoved
	}
	data, info, err := f.read()
	if err != nil {
		return "", "", err
	}
	if !os.SameFile(info, f.info) || sha256.Sum256(data) != f.digest {
		return "", "", coreauth.ErrFileRemovalBusy
	}
	return copilotFileIdentity(data)
}

type accountSettingsPatchKey struct{}

type accountSettingsPatch struct {
	file                 AccountSettingsFile
	note, disabled       bool
	refreshEnabled       bool
	fields               []string
	selectedClaudeLogin  bool
	beginClaudeWrite     func() (release func(), eligible bool)
	selectedCodexLogin   bool
	selectedCopilotLogin bool
}

// WithAccountSettingsFilePatch lets a local store preserve unedited JSON bytes.
// Other stores still use their normal ModifyPersisted transaction.
func WithAccountSettingsFilePatch(ctx context.Context, file AccountSettingsFile, note, disabled bool, refresh ...bool) context.Context {
	return context.WithValue(ctx, accountSettingsPatchKey{}, accountSettingsPatch{file: file, note: note, disabled: disabled, refreshEnabled: len(refresh) > 0 && refresh[0]})
}

// WithCredentialFileFieldsPatch gives FileTokenStore the top-level fields
// touched by an explicit edit. Other stores still use their normal transaction.
func WithCredentialFileFieldsPatch(ctx context.Context, fields []string) context.Context {
	return context.WithValue(ctx, accountSettingsPatchKey{}, accountSettingsPatch{fields: fields})
}

// WithSelectedCodexLoginFile binds a replacement grant to the file seen when
// the operator opened a reauthentication session. A removed file cannot be recreated.
func WithSelectedCodexLoginFile(ctx context.Context, file AccountSettingsFile) context.Context {
	return context.WithValue(ctx, accountSettingsPatchKey{}, accountSettingsPatch{file: file, selectedCodexLogin: true})
}

// WithSelectedCopilotLoginFile binds a verified GitHub grant to one inspected file.
func WithSelectedCopilotLoginFile(ctx context.Context, file AccountSettingsFile) context.Context {
	return context.WithValue(ctx, accountSettingsPatchKey{}, accountSettingsPatch{file: file, selectedCopilotLogin: true})
}

// WithSelectedClaudeLoginFile binds a replacement grant to the inspected file.
// beginWrite fences the original directory while the local write is installed.
func WithSelectedClaudeLoginFile(ctx context.Context, file AccountSettingsFile, beginWrite func() (func(), bool)) context.Context {
	return context.WithValue(ctx, accountSettingsPatchKey{}, accountSettingsPatch{
		file: file, selectedClaudeLogin: true, beginClaudeWrite: beginWrite,
	})
}

func HasSelectedClaudeLoginFile(ctx context.Context) bool {
	patch, ok := explicitAccountSettingsPatch(ctx)
	return ok && patch.selectedClaudeLogin
}

// MatchesClaudeLoginOwner verifies that the inspected file still identifies
// the selected provider account before a reauthentication session is issued.
func (f AccountSettingsFile) MatchesClaudeLoginOwner(email, account, organization string) bool {
	if !f.Valid() {
		return false
	}
	raw, info, err := f.read()
	return err == nil && os.SameFile(info, f.info) && sha256.Sum256(raw) == f.digest &&
		matchesClaudeLoginOwner(raw, email, account, organization)
}

// ReadSelectedClaudeFile validates a newly committed file snapshot using the
// same scoped path as the pre-login selection.
func (f AccountSettingsFile) ReadSelectedClaudeFile(path string) ([]byte, error) {
	absolute, err := filepath.Abs(path)
	if err != nil || !f.Valid() || absolute != f.path {
		return nil, coreauth.ErrCredentialFileRemoved
	}
	data, _, err := f.read()
	return data, err
}

func matchesClaudeLoginOwner(raw []byte, email, account, organization string) bool {
	var prior struct {
		Type         string `json:"type"`
		Email        string `json:"email"`
		Account      string `json:"account_uuid"`
		Organization string `json:"organization_uuid"`
	}
	if json.Unmarshal(raw, &prior) != nil || !strings.EqualFold(strings.TrimSpace(prior.Type), "claude") {
		return false
	}
	priorEmail := strings.TrimSpace(prior.Email)
	// A provider's email can be reused or shared. Only the stable account
	// recorded before this session can authorize replacing the saved grant.
	return strings.TrimSpace(prior.Account) != "" && EqualClaudeOwnerID(prior.Account, account) &&
		(priorEmail == "" || strings.EqualFold(priorEmail, strings.TrimSpace(email))) &&
		(strings.TrimSpace(prior.Organization) == "" || EqualClaudeOwnerID(prior.Organization, organization))
}

// EqualClaudeOwnerID trims surrounding whitespace and accepts hex case
// differences only for standard UUIDs. Trimmed opaque IDs remain case-sensitive.
func EqualClaudeOwnerID(left, right string) bool {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	if left == right {
		return true
	}
	const uuidTextLength = 36
	if len(left) != uuidTextLength || len(right) != uuidTextLength {
		return false
	}
	leftUUID, err := uuid.Parse(left)
	if err != nil {
		return false
	}
	rightUUID, err := uuid.Parse(right)
	return err == nil && leftUUID == rightUUID
}

func (patch accountSettingsPatch) selectedLogin() bool {
	return patch.selectedClaudeLogin || patch.selectedCodexLogin || patch.selectedCopilotLogin
}

func explicitAccountSettingsPatch(ctx context.Context) (accountSettingsPatch, bool) {
	if ctx == nil {
		return accountSettingsPatch{}, false
	}
	patch, ok := ctx.Value(accountSettingsPatchKey{}).(accountSettingsPatch)
	return patch, ok && (patch.selectedLogin() || coreauth.IsExplicitCredentialEdit(ctx))
}

func (s *FileTokenStore) saveAccountSettingsPatch(path string, auth *coreauth.Auth, patch accountSettingsPatch) (string, error) {
	if patch.selectedLogin() && !patch.file.Valid() {
		return "", coreauth.ErrCredentialFileRemoved
	}
	if patch.selectedClaudeLogin {
		if patch.beginClaudeWrite == nil {
			return "", coreauth.ErrAuthPublicationOutOfScope
		}
		release, eligible := patch.beginClaudeWrite()
		if !eligible {
			return "", coreauth.ErrAuthPublicationOutOfScope
		}
		release()
	}
	absolute, err := filepath.Abs(path)
	if err != nil || (patch.file.Valid() && absolute != patch.file.path) {
		return "", coreauth.ErrFileRemovalBusy
	}
	mu := s.writeMutex()
	mu.Lock()
	defer mu.Unlock()
	dir := strings.TrimSpace(s.baseDirSnapshot())
	if dir == "" {
		return "", coreauth.ErrCredentialFileRemoved
	}
	if !patch.file.Valid() {
		patch.file, err = ReadAccountSettingsFile(dir, absolute)
		if err != nil {
			return "", err
		}
	}
	baseInfo, err := os.Stat(dir)
	if err != nil {
		return "", coreauth.ErrCredentialFileRemoved
	}
	requestInfo, err := os.Stat(patch.file.dir)
	if err != nil || !os.SameFile(baseInfo, requestInfo) {
		return "", coreauth.ErrFileRemovalBusy
	}
	lockedPath, err := ordinaryAuthWritePath(absolute)
	if err != nil {
		return "", err
	}
	var persistedMetadata map[string]any
	err = withAccountSettingsFileLock(lockedPath, func() error {
		verify := func() ([]byte, error) {
			data, info, err := patch.file.read()
			if err != nil {
				return nil, err
			}
			if !os.SameFile(info, patch.file.info) || sha256.Sum256(data) != patch.file.digest {
				return nil, coreauth.ErrFileRemovalBusy
			}
			return data, nil
		}
		old, err := verify()
		if err != nil {
			return err
		}
		fields := make(map[string]json.RawMessage, len(patch.fields)+2)
		if patch.selectedClaudeLogin {
			grant, ok := auth.Storage.(*claude.ClaudeTokenStorage)
			if !ok || grant == nil || !strings.EqualFold(auth.Provider, "claude") ||
				strings.TrimSpace(grant.AccessToken) == "" || strings.TrimSpace(grant.RefreshToken) == "" ||
				!matchesClaudeLoginOwner(old, grant.Email, grant.AccountUUID, grant.OrganizationUUID) {
				return coreauth.ErrFileRemovalBusy
			}
			for name, value := range map[string]string{
				"id_token": grant.IDToken, "access_token": grant.AccessToken, "refresh_token": grant.RefreshToken,
				"expired": grant.Expire, "last_refresh": grant.LastRefresh, "email": grant.Email,
				"account_uuid": grant.AccountUUID, "organization_uuid": grant.OrganizationUUID,
				"organization_name": grant.OrganizationName,
			} {
				if value == "" && name != "id_token" {
					continue
				}
				fields[name], err = json.Marshal(value)
				if err != nil {
					return err
				}
			}
			var prior map[string]any
			if err := json.Unmarshal(old, &prior); err != nil {
				return err
			}
			if coreauth.IsReauthRequiredMetadata(prior) {
				for _, name := range []string{"reauth_required", "refresh_status", "refresh_error_code",
					"refresh_disabled_reason", "refresh_disabled", "refresh_disabled_at", "last_refresh_error"} {
					fields[name] = nil
				}
			}
			if quarantined, _ := prior["auto_quarantined"].(bool); quarantined && prior["quarantine_reason"] == "terminal_auth_failure" {
				for _, name := range []string{"auto_quarantined", "quarantine_reason", "quarantined_at"} {
					fields[name] = nil
				}
			}
		}
		if patch.selectedCodexLogin {
			grant, ok := auth.Storage.(*codex.CodexTokenStorage)
			if !ok || grant == nil || !coreauth.IsCodexAuthRecord(auth) {
				return errors.New("selected credential requires a Codex login grant")
			}
			var prior struct {
				Type      string `json:"type"`
				Email     string `json:"email"`
				AccountID string `json:"account_id"`
			}
			if err := json.Unmarshal(old, &prior); err != nil {
				return err
			}
			if prior.Type != "" && !strings.EqualFold(prior.Type, "codex") ||
				prior.AccountID != "" && prior.AccountID != grant.AccountID ||
				prior.Email != "" && !strings.EqualFold(strings.TrimSpace(prior.Email), strings.TrimSpace(grant.Email)) {
				return coreauth.ErrFileRemovalBusy
			}
			values, err := grant.ReauthenticationFields()
			if err != nil {
				return err
			}
			for name, value := range values {
				fields[name], err = json.Marshal(value)
				if err != nil {
					return err
				}
			}
		}
		if patch.selectedCopilotLogin {
			subject, ok := CopilotAccountID(auth.Metadata["account_id"])
			prior, _, err := copilotFileIdentity(old)
			if err != nil || !ok || subject != prior || !strings.EqualFold(auth.Provider, copilot.Provider) || auth.Storage != nil {
				return coreauth.ErrFileRemovalBusy
			}
			for _, name := range []string{"access_token", "refresh_token", "username", "base_url", "expired"} {
				value, ok := auth.Metadata[name].(string)
				if !ok || strings.TrimSpace(value) == "" {
					return errors.New("selected credential requires a complete Copilot login grant")
				}
				fields[name], err = json.Marshal(value)
				if err != nil {
					return err
				}
			}
		}
		for _, name := range patch.fields {
			value, exists := auth.Metadata[name]
			if name == "disabled" {
				value, exists = auth.Disabled, true
			}
			if !exists {
				fields[name] = nil
				continue
			}
			fields[name], err = json.Marshal(value)
			if err != nil {
				return err
			}
		}
		if patch.note {
			note, ok := auth.Metadata["note"].(string)
			if !ok {
				return errors.New("invalid ordinary note")
			}
			fields["note"], err = json.Marshal(note)
			if err != nil {
				return err
			}
		}
		if patch.disabled {
			fields["disabled"], err = json.Marshal(auth.Disabled)
			if err != nil {
				return err
			}
		}
		if patch.refreshEnabled {
			settings, _ := auth.Metadata["account_settings"].(map[string]any)
			enabled, ok := settings["refresh_enabled"].(bool)
			if !ok {
				return errors.New("invalid ordinary refresh setting")
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(old, &raw); err != nil {
				return err
			}
			object := raw["account_settings"]
			if len(object) == 0 || bytes.Equal(object, []byte("null")) {
				object = []byte("{}")
			}
			if enabled && !auth.AutoQuarantined && !coreauth.IsReauthRequiredMetadata(auth.Metadata) {
				for _, key := range []string{"refresh_disabled", "disable_refresh", "auto_refresh_disabled", "refresh_enabled"} {
					fields[key] = nil
				}
			}
			value := json.RawMessage(strconv.FormatBool(enabled))
			fields["account_settings"], err = patchCredentialFileJSON(object, map[string]json.RawMessage{"refresh_enabled": value})
			if err != nil {
				return err
			}
		}
		data, err := patchCredentialFileJSON(old, fields)
		if err != nil {
			return err
		}
		if patch.selectedLogin() {
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.UseNumber()
			if err := dec.Decode(&persistedMetadata); err != nil {
				return err
			}
		}
		if patch.selectedClaudeLogin {
			// Upload takes the credential lock before its directory fence.
			// Use the same order and hold no directory lock across callbacks.
			release, eligible := patch.beginClaudeWrite()
			if !eligible {
				return coreauth.ErrAuthPublicationOutOfScope
			}
			defer release()
		}
		if bytes.Equal(data, old) {
			return nil
		}

		file, err := os.OpenFile(lockedPath, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer file.Close()
		guard, err := guardOrdinaryCredentialMetadata(file, lockedPath)
		if err != nil {
			return err
		}
		if guard == nil {
			// Platforms without the extended metadata guard still preserve
			// the precise existing permission mode and check the same file.
			guard = &ordinaryCredentialMetadataGuard{
				checkTemporary: func(tmp *os.File) error { return tmp.Chmod(0600) },
				setMetadata:    func(tmp *os.File) error { return tmp.Chmod(patch.file.info.Mode().Perm()) },
				checkTarget:    func() error { return nil },
			}
		}
		checkMetadata := guard.checkTarget
		guard.checkTarget = func() error {
			if err := checkMetadata(); err != nil {
				return err
			}
			_, err := verify()
			return err
		}
		return writeAccountSettingsWithHistory(lockedPath, old, data, guard)
	})
	if err != nil {
		return "", err
	}
	if patch.selectedLogin() {
		auth.Metadata = persistedMetadata
		coreauth.NormalizeCredentialMetadata(auth.Metadata)
		if patch.selectedCopilotLogin {
			route, _ := auth.Metadata["proxy_url"].(string)
			auth.ProxyURL = strings.TrimSpace(route)
		}
		if disabled, ok := auth.Metadata["disabled"].(bool); ok {
			auth.Disabled = disabled
		}
	}
	return path, nil
}

type credentialJSONMember struct {
	name                       string
	before, keyStart, valueEnd int
	value                      json.RawMessage
}

// patchCredentialFileJSON changes only selected members in the original object.
// A nil replacement deletes a member. A JSON null sets its value to null.
func patchCredentialFileJSON(src []byte, fields map[string]json.RawMessage) ([]byte, error) {
	if !json.Valid(src) {
		return nil, errors.New("invalid credential JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(src))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.New("credential JSON must be an object")
	}
	seen := make(map[string]bool)
	var members []credentialJSONMember
	for dec.More() {
		before := int(dec.InputOffset())
		keyStart := before
		for keyStart < len(src) && src[keyStart] != '"' {
			keyStart++
		}
		tok, err := dec.Token()
		name, ok := tok.(string)
		if err != nil || !ok || seen[name] {
			return nil, errors.New("credential JSON must use unique object fields")
		}
		seen[name] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		members = append(members, credentialJSONMember{name, before, keyStart, int(dec.InputOffset()), value})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	closing := int(dec.InputOffset()) - 1

	deleted := func(member credentialJSONMember) bool {
		key := coreauth.CanonicalCredentialMetadataKey(member.name)
		replacement, edit := fields[key]
		return edit && (replacement == nil || member.name != key)
	}
	var out bytes.Buffer
	last, retained := 0, 0
	for i := 0; i < len(members); {
		member := members[i]
		if deleted(member) {
			end := i + 1
			for end < len(members) && deleted(members[end]) {
				end++
			}
			begin, after := member.before, members[end-1].valueEnd
			if i == 0 {
				begin = member.keyStart
				if end < len(members) {
					after = members[end].keyStart
				}
			}
			out.Write(src[last:begin])
			last, i = after, end
			continue
		}
		if value, edit := fields[member.name]; edit && !bytes.Equal(member.value, value) {
			out.Write(src[last : member.valueEnd-len(member.value)])
			out.Write(value)
			last = member.valueEnd
		}
		retained++
		i++
	}
	out.Write(src[last:closing])
	var added []string
	for name, value := range fields {
		if !seen[name] && value != nil {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	for _, name := range added {
		if retained > 0 {
			out.WriteByte(',')
		}
		retained++
		key, _ := json.Marshal(name)
		out.Write(key)
		out.WriteByte(':')
		out.Write(fields[name])
	}
	out.Write(src[closing:])
	return out.Bytes(), nil
}
