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
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
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

type accountSettingsPatchKey struct{}

type accountSettingsPatch struct {
	file               AccountSettingsFile
	note, disabled     bool
	fields             []string
	selectedCodexLogin bool
}

// WithAccountSettingsFilePatch lets a local store preserve unedited JSON bytes.
// Other stores still use their normal ModifyPersisted transaction.
func WithAccountSettingsFilePatch(ctx context.Context, file AccountSettingsFile, note, disabled bool) context.Context {
	return context.WithValue(ctx, accountSettingsPatchKey{}, accountSettingsPatch{file: file, note: note, disabled: disabled})
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

func explicitAccountSettingsPatch(ctx context.Context) (accountSettingsPatch, bool) {
	if ctx == nil {
		return accountSettingsPatch{}, false
	}
	patch, ok := ctx.Value(accountSettingsPatchKey{}).(accountSettingsPatch)
	return patch, ok && (patch.selectedCodexLogin || coreauth.IsExplicitCredentialEdit(ctx))
}

func (s *FileTokenStore) saveAccountSettingsPatch(path string, auth *coreauth.Auth, patch accountSettingsPatch) (string, error) {
	if patch.selectedCodexLogin && !patch.file.Valid() {
		return "", coreauth.ErrCredentialFileRemoved
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
		data, err := patchCredentialFileJSON(old, fields)
		if err != nil {
			return err
		}
		if patch.selectedCodexLogin {
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.UseNumber()
			if err := dec.Decode(&persistedMetadata); err != nil {
				return err
			}
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
	if patch.selectedCodexLogin {
		auth.Metadata = persistedMetadata
		coreauth.NormalizeCredentialMetadata(auth.Metadata)
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
