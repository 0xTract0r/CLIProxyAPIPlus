package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// PluginAuthParser parses auth JSON owned by plugin providers.
type PluginAuthParser interface {
	ParseAuth(context.Context, pluginapi.AuthParseRequest) (*cliproxyauth.Auth, bool, error)
}

// PluginMultiAuthParser expands one auth JSON payload into multiple plugin auth records.
// Returning handled=true with an empty slice means the plugin intentionally suppresses built-in parsing.
type PluginMultiAuthParser interface {
	ParseAuths(context.Context, pluginapi.AuthParseRequest) ([]*cliproxyauth.Auth, bool, error)
}

type pluginAuthParserHolder struct {
	parser PluginAuthParser
}

var pluginAuthParserValue atomic.Value

// RegisterPluginAuthParser registers the current plugin auth parser.
func RegisterPluginAuthParser(parser PluginAuthParser) {
	pluginAuthParserValue.Store(pluginAuthParserHolder{parser: parser})
}

func currentPluginAuthParser() PluginAuthParser {
	value := pluginAuthParserValue.Load()
	if value == nil {
		return nil
	}
	holder, ok := value.(pluginAuthParserHolder)
	if !ok {
		return nil
	}
	return holder.parser
}

// FileTokenStore persists token records and auth metadata using the filesystem as backing storage.
type FileTokenStore struct {
	mu            sync.Mutex
	sharedWriteMu *sync.Mutex
	dirLock       sync.RWMutex
	baseDir       string
}

// NewFileTokenStore creates a token store that saves credentials to disk through the
// TokenStorage implementation embedded in the token record.
func NewFileTokenStore() *FileTokenStore {
	return &FileTokenStore{}
}

// WithBaseDir returns a directory view of this store. Views share the
// original store's write lock, but changing a view's directory is local to it.
func (s *FileTokenStore) WithBaseDir(dir string) *FileTokenStore {
	return &FileTokenStore{
		baseDir:       strings.TrimSpace(dir),
		sharedWriteMu: s.writeMutex(),
	}
}

func (s *FileTokenStore) writeMutex() *sync.Mutex {
	if s.sharedWriteMu != nil {
		return s.sharedWriteMu
	}
	return &s.mu
}

// SetBaseDir updates the default directory used for auth JSON persistence when no explicit path is provided.
func (s *FileTokenStore) SetBaseDir(dir string) {
	s.dirLock.Lock()
	s.baseDir = strings.TrimSpace(dir)
	s.dirLock.Unlock()
}

// Save persists token storage and metadata to the resolved auth file path.
func (s *FileTokenStore) Save(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("auth filestore: auth is nil")
	}
	cliproxyauth.NormalizeCredentialMetadata(auth.Metadata)
	if errWeight := cliproxyauth.ValidateAuthWeight(auth); errWeight != nil {
		return "", fmt.Errorf("auth filestore: %w", errWeight)
	}

	path, err := s.resolveAuthPath(auth)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("auth filestore: missing file path attribute for %s", auth.ID)
	}

	if patch, ok := explicitAccountSettingsPatch(ctx); ok &&
		(patch.selectedLogin() || patch.file.Valid() || (auth.Storage == nil && !cliproxyauth.IsCodexAuthRecord(auth))) {
		return s.saveAccountSettingsPatch(path, auth, patch)
	}

	// Runtime updates must not recreate a disabled credential whose source file
	// was deliberately removed. Login and migration callers explicitly mark the
	// save when creating a missing disabled credential is intentional.
	explicitEdit := cliproxyauth.IsExplicitCredentialEdit(ctx)
	xaiLogin := cliproxyauth.HasAuthCreationIntent(ctx) && strings.EqualFold(auth.Provider, "xai")
	if !explicitEdit && auth.Disabled && !cliproxyauth.HasAuthCreationIntent(ctx) {
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			return "", nil
		}
	}

	writeMu := s.writeMutex()
	writeMu.Lock()
	defer writeMu.Unlock()

	if explicitEdit {
		if err := s.requireScopedExistingAuthFile(path); err != nil {
			return "", err
		}
	}

	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("auth filestore: create dir failed: %w", err)
	}

	// 专用持久化自行获取写锁，保留其原有的快照时机。
	if cliproxyauth.IsCodexAuthRecord(auth) {
		snapshotAuthFileHistory(path)
	}

	// metadataSetter is a private interface for TokenStorage implementations that support metadata injection.
	type metadataSetter interface {
		SetMetadata(map[string]any)
	}

	if handled, errCodex := cliproxyauth.PersistCodexAuthFile(ctx, path, auth); handled {
		if errCodex != nil {
			return "", errCodex
		}
	} else {
		lockPath := path
		if xaiLogin || (auth.Storage == nil && auth.Metadata != nil) {
			lockPath, err = ordinaryAuthWritePath(path)
			if err != nil {
				return "", err
			}
		}
		// 非 Codex 覆盖也可能替换仍有 SIWC 写入的 Codex 文件。
		// 与上面的 Codex 持久化一致：先 store mutex，后 file lock；不能在 Save 外套锁。
		var plan *XAILoginPersistence
		if xaiLogin {
			plan, _ = ctx.Value(xaiLoginPlanKey{}).(*XAILoginPersistence)
		}
		if err = codex.WithFileLock(lockPath, func() error {
			if xaiLogin {
				current, errPath := ordinaryAuthWritePath(path)
				if errPath != nil {
					return errPath
				}
				if current != lockPath {
					return cliproxyauth.ErrFileRemovalBusy
				}
				existing, errTarget := xaiLoginDestination(path, auth.Metadata)
				if errTarget != nil {
					return errTarget
				}
				if plan != nil {
					if err := plan.verifyLocked(lockPath, existing); err != nil {
						return err
					}
				}
			}
			switch {
			case auth.Storage != nil:
				if auth.Metadata == nil {
					auth.Metadata = make(map[string]any)
				}
				auth.Metadata["disabled"] = auth.Disabled
				if setter, ok := auth.Storage.(metadataSetter); ok {
					setter.SetMetadata(auth.Metadata)
				}
				snapshotAuthFileHistory(path)
				if err = auth.Storage.SaveTokenToFile(path); err != nil {
					return err
				}
			case auth.Metadata != nil:
				current, errPath := ordinaryAuthWritePath(path)
				if errPath != nil {
					return errPath
				}
				if current != lockPath {
					if _, statErr := os.Stat(path); explicitEdit && os.IsNotExist(statErr) {
						return cliproxyauth.ErrCredentialFileRemoved
					}
					return cliproxyauth.ErrFileRemovalBusy
				}
				auth.Metadata["disabled"] = auth.Disabled
				raw, errMarshal := json.Marshal(auth.Metadata)
				if errMarshal != nil {
					return fmt.Errorf("auth filestore: marshal metadata failed: %w", errMarshal)
				}
				if existing, errRead := os.ReadFile(path); errRead == nil {
					if jsonEqual(existing, raw) {
						break
					}
				} else if !os.IsNotExist(errRead) {
					return fmt.Errorf("auth filestore: read existing failed: %w", errRead)
				} else if explicitEdit {
					return cliproxyauth.ErrCredentialFileRemoved
				}
				snapshotAuthFileHistory(path)
				if errWrite := writeFileAtomic(path, raw, 0o600); errWrite != nil {
					return fmt.Errorf("auth filestore: write file failed: %w", errWrite)
				}

			default:
				return fmt.Errorf("auth filestore: nothing to persist for %s", auth.ID)
			}
			return nil
		}); err != nil {
			return "", err
		}
	}

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[cliproxyauth.AttributePath] = path
	auth.Attributes[cliproxyauth.AttributeSource] = path
	auth.Attributes[cliproxyauth.AttributeSourceBackend] = cliproxyauth.AuthSourceFile

	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = auth.ID
	}

	return path, nil
}

// authFileHistoryDirName is the per-auth-directory subfolder that stores rolling
// whole-file snapshots of auth JSON before each write.
const authFileHistoryDirName = ".history"

// authFileHistoryKeep is the number of most recent whole-file snapshots retained
// per auth file. Older snapshots beyond this window are pruned.
const authFileHistoryKeep = 7

const authFileHistoryTimestamp = "20060102T150405.000000000"

// snapshotAuthFileHistory copies the current contents of an auth JSON file into a
// permission-tightened history subdirectory before it is overwritten, keeping the
// most recent authFileHistoryKeep snapshots and pruning older ones. It is
// best-effort: any error is ignored so it never blocks persisting auth state. A
// snapshot is only taken when the target file already exists and is non-empty.
func snapshotAuthFileHistory(path string) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return
	}

	baseName := filepath.Base(path)
	historyDir := filepath.Join(filepath.Dir(path), authFileHistoryDirName)
	if errMk := os.MkdirAll(historyDir, 0o700); errMk != nil {
		return
	}

	info, errStat := os.Lstat(historyDir)
	if errStat != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return
	}

	// Snapshot name encodes the source file and a high-resolution timestamp so the
	// lexical order matches chronological order for pruning.
	snapshotName := fmt.Sprintf("%s.%s.bak", baseName, time.Now().UTC().Format(authFileHistoryTimestamp))
	snapshotPath := filepath.Join(historyDir, snapshotName)
	if errWrite := writeFileAtomic(snapshotPath, data, 0o600); errWrite != nil {
		return
	}

	pruneAuthFileHistory(historyDir, baseName)
}

// pruneAuthFileHistory removes snapshots for baseName beyond the retention window,
// keeping the authFileHistoryKeep most recent entries. Errors are ignored.
func pruneAuthFileHistory(historyDir, baseName string) {
	entries, err := os.ReadDir(historyDir)
	if err != nil {
		return
	}
	prefix := baseName + "."
	snapshots := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".bak") {
			stamp := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".bak")
			if parsed, err := time.Parse(authFileHistoryTimestamp, stamp); err == nil && parsed.Format(authFileHistoryTimestamp) == stamp {
				snapshots = append(snapshots, name)
			}
		}
	}
	if len(snapshots) <= authFileHistoryKeep {
		return
	}
	// Names embed a zero-padded timestamp, so ascending lexical order is oldest-first.
	sort.Strings(snapshots)
	for _, name := range snapshots[:len(snapshots)-authFileHistoryKeep] {
		_ = os.Remove(filepath.Join(historyDir, name))
	}
}

// writeFileAtomic writes data to path atomically: it creates a temp file in the
// same directory, writes and fsyncs it, then renames it over the destination.
// A crash or write error therefore never exposes a truncated or empty auth file
// — the previous contents survive intact until the rename completes. The temp
// file is created with perm and cleaned up if the rename does not happen.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup; after a successful rename tmpName no longer exists and
	// the Remove simply fails silently.
	defer func() { _ = os.Remove(tmpName) }()

	if err = tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// List enumerates all auth JSON files under the configured directory.
func (s *FileTokenStore) List(ctx context.Context) ([]*cliproxyauth.Auth, error) {
	dir := s.baseDirSnapshot()
	if dir == "" {
		return nil, fmt.Errorf("auth filestore: directory not configured")
	}
	entries := make([]*cliproxyauth.Auth, 0)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != dir && (strings.EqualFold(d.Name(), "incident-archives") || d.Name() == authFileHistoryDirName) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
			return nil
		}
		auths, errReadAuths := s.readAuthFiles(path, dir)
		if errReadAuths != nil {
			return nil
		}
		if len(auths) > 0 {
			entries = append(entries, auths...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func ordinaryAuthWritePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved, nil
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(absolute)); err == nil {
		absolute = filepath.Join(parent, filepath.Base(absolute))
	}
	return absolute, nil
}

// WithExistingAuthFile serializes an unlink with credential writes to the same
// normalized path. The callback gets the latest directory entry and must act
// on the original path, so removing a symbolic link leaves its target intact.
func WithExistingAuthFile(path string, remove func(os.FileInfo) error) error {
	// Avoid creating a missing credential directory just to lock a no-op.
	if _, err := os.Lstat(path); err != nil {
		return err
	}
	target, err := ordinaryAuthWritePath(path)
	if err != nil {
		return err
	}
	return codex.WithFileLock(target, func() error {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		current, err := ordinaryAuthWritePath(path)
		if err != nil {
			return err
		}
		if current != target {
			return cliproxyauth.ErrFileRemovalBusy
		}
		return remove(info)
	})
}

// Delete removes the auth file.
func (s *FileTokenStore) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("auth filestore: id is empty")
	}
	path, err := s.resolveDeletePath(id)
	if err != nil {
		return err
	}
	err = WithExistingAuthFile(path, func(os.FileInfo) error {
		return os.Remove(path)
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("auth filestore: delete failed: %w", err)
	}
	return nil
}

func (s *FileTokenStore) resolveDeletePath(id string) (string, error) {
	if strings.ContainsRune(id, os.PathSeparator) || filepath.IsAbs(id) {
		return id, nil
	}
	dir := s.baseDirSnapshot()
	if dir == "" {
		return "", fmt.Errorf("auth filestore: directory not configured")
	}
	return filepath.Join(dir, id), nil
}

func (s *FileTokenStore) readAuthFiles(path, baseDir string) ([]*cliproxyauth.Auth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}
	metadata, err := cliproxyauth.DecodeCredentialMetadata(data)
	if err != nil {
		return nil, fmt.Errorf("unmarshal auth json: %w", err)
	}
	cliproxyauth.NormalizeCredentialMetadata(metadata)
	if errWeight := cliproxyauth.ValidateAuthWeight(&cliproxyauth.Auth{Metadata: metadata}); errWeight != nil {
		return nil, errWeight
	}
	provider, _ := metadata["type"].(string)
	provider = strings.TrimSpace(provider)
	if strings.EqualFold(provider, "gemini") {
		return nil, nil
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		return nil, fmt.Errorf("stat file: %w", errStat)
	}
	if parser := currentPluginAuthParser(); parser != nil {
		auths, handled, errParse := parsePluginAuthFile(parser, pluginapi.AuthParseRequest{
			Provider: provider,
			Path:     path,
			FileName: s.idFor(path, baseDir),
			RawJSON:  data,
		})
		if errParse == nil && handled {
			auths = compactPluginAuths(auths)
			if len(auths) == 0 {
				return nil, nil
			}
			disabled, _ := metadata["disabled"].(bool)
			for index, auth := range auths {
				if auth == nil {
					continue
				}
				cliproxyauth.NormalizeCredentialMetadata(auth.Metadata)
				if len(auths) > 1 {
					cliproxyauth.MarkPluginVirtualAuth(auth, path, index)
				}
				auth.CreatedAt = info.ModTime()
				auth.UpdatedAt = info.ModTime()
				if auth.Attributes == nil {
					auth.Attributes = make(map[string]string)
				}
				auth.Attributes[cliproxyauth.AttributePath] = path
				auth.Attributes[cliproxyauth.AttributeSource] = path
				auth.Attributes[cliproxyauth.AttributeSourceBackend] = cliproxyauth.AuthSourceFile
				if disabled {
					auth.Disabled = true
					auth.Status = cliproxyauth.StatusDisabled
					if auth.Metadata == nil {
						auth.Metadata = make(map[string]any)
					}
					auth.Metadata["disabled"] = true
				}
				if p, ok := metadata["proxy_url"].(string); ok && auth.ProxyURL == "" {
					auth.ProxyURL = strings.TrimSpace(p)
				}
				if pref, ok := metadata["prefix"].(string); ok && auth.Prefix == "" {
					auth.Prefix = strings.Trim(strings.TrimSpace(pref), "/")
				}
				if errWeight := cliproxyauth.ApplyAuthWeightMetadata(auth, metadata); errWeight != nil {
					return nil, errWeight
				}
				cliproxyauth.ApplyAuthPriorityMetadata(auth, metadata)
				if _, inherited := auth.Attributes[cliproxyauth.AttributeFilePriority]; inherited {
					if setter, ok := auth.Storage.(interface{ SetMetadata(map[string]any) }); ok {
						setter.SetMetadata(auth.Metadata)
					}
				}
				cliproxyauth.ApplyCustomHeadersFromMetadata(auth)
			}
			return auths, nil
		}
	}
	if provider == "" {
		provider = "unknown"
	}
	info, errStat = os.Stat(path)
	if errStat != nil {
		return nil, fmt.Errorf("stat file: %w", errStat)
	}
	id := s.idFor(path, baseDir)
	disabled, _ := metadata["disabled"].(bool)
	// autoQuarantined restores the fork's terminal-auth-failure quarantine
	// lock (see markAutoQuarantine/clearAutoQuarantine in
	// sdk/cliproxy/auth/conductor_auto_quarantine.go) from the persisted
	// Metadata key so it survives a process restart. Without this, the lock
	// only ever lived on the in-memory Auth struct and a restart silently
	// re-exposed a terminally revoked credential as a fresh, active one.
	autoQuarantined, _ := metadata["auto_quarantined"].(bool)
	status := cliproxyauth.StatusActive
	switch {
	case disabled:
		// An operator-disabled account keeps StatusDisabled for display even
		// if it was also auto-quarantined before being disabled (operators
		// can disable an already-quarantined credential; see
		// applyAuthDisabledState, which does not clear an existing
		// quarantine on disable). AutoQuarantined is still set below
		// regardless, so the selector's isAuthBlockedForModel OR-check
		// blocks on either reason independently.
		status = cliproxyauth.StatusDisabled
	case autoQuarantined:
		status = cliproxyauth.StatusQuarantined
	}

	// Calculate NextRefreshAfter from expires_at (20 minutes before expiry)
	var nextRefreshAfter time.Time
	if expiresAtStr, ok := metadata["expires_at"].(string); ok && expiresAtStr != "" {
		if expiresAt, err := time.Parse(time.RFC3339, expiresAtStr); err == nil {
			nextRefreshAfter = expiresAt.Add(-20 * time.Minute)
		}
	}

	proxyURL := ""
	if p, ok := metadata["proxy_url"].(string); ok {
		proxyURL = strings.TrimSpace(p)
	}
	prefix := ""
	if rawPrefix, ok := metadata["prefix"].(string); ok {
		trimmed := strings.TrimSpace(rawPrefix)
		trimmed = strings.Trim(trimmed, "/")
		if trimmed != "" && !strings.Contains(trimmed, "/") {
			prefix = trimmed
		}
	}
	auth := &cliproxyauth.Auth{
		ID:              id,
		Provider:        provider,
		FileName:        id,
		Label:           s.labelFor(metadata),
		Status:          status,
		Disabled:        disabled,
		Prefix:          prefix,
		ProxyURL:        proxyURL,
		AutoQuarantined: autoQuarantined,
		// Unavailable mirrors markAutoQuarantine's own in-memory state so a
		// restored record is byte-for-byte consistent with a live-quarantined
		// one, the same invariant preserveQuarantineFieldsOnStaleWriteback
		// already protects against a stale write-back rolling back.
		Unavailable: autoQuarantined,
		Attributes: map[string]string{
			cliproxyauth.AttributePath:          path,
			cliproxyauth.AttributeSource:        path,
			cliproxyauth.AttributeSourceBackend: cliproxyauth.AuthSourceFile,
		},
		Metadata:         metadata,
		CreatedAt:        info.ModTime(),
		UpdatedAt:        info.ModTime(),
		LastRefreshedAt:  time.Time{},
		NextRefreshAfter: nextRefreshAfter,
	}
	if email, ok := metadata["email"].(string); ok && email != "" {
		auth.Attributes["email"] = email
	}
	if autoQuarantined {
		if reason, ok := metadata["quarantine_reason"].(string); ok {
			auth.QuarantineReason = reason
		}
		if quarantinedAtStr, ok := metadata["quarantined_at"].(string); ok && quarantinedAtStr != "" {
			if quarantinedAt, err := time.Parse(time.RFC3339, quarantinedAtStr); err == nil {
				auth.QuarantinedAt = quarantinedAt
			}
		}
	}
	cliproxyauth.ApplyCustomHeadersFromMetadata(auth)
	// Restore the terminal reauth-required lock (persisted by
	// markRefreshReauthRequiredWithReason) the same way autoQuarantined is
	// restored just above, so a dead refresh token survives a process restart
	// as an abnormal, unroutable record instead of a fresh StatusActive one.
	// It is a no-op when disabled/auto-quarantined already own a stronger
	// terminal Status (see ApplyReauthRequiredStateFromMetadata).
	cliproxyauth.ApplyReauthRequiredStateFromMetadata(auth)
	return []*cliproxyauth.Auth{auth}, nil
}

func (s *FileTokenStore) readAuthFile(path, baseDir string) (*cliproxyauth.Auth, error) {
	auths, errReadAuths := s.readAuthFiles(path, baseDir)
	if errReadAuths != nil || len(auths) == 0 {
		return nil, errReadAuths
	}
	return auths[0], nil
}

func parsePluginAuthFile(parser PluginAuthParser, req pluginapi.AuthParseRequest) ([]*cliproxyauth.Auth, bool, error) {
	if parser == nil {
		return nil, false, nil
	}
	if multiParser, ok := parser.(PluginMultiAuthParser); ok {
		return multiParser.ParseAuths(context.Background(), req)
	}
	auth, handled, errParse := parser.ParseAuth(context.Background(), req)
	if errParse != nil || !handled || auth == nil {
		return nil, handled, errParse
	}
	return []*cliproxyauth.Auth{auth}, true, nil
}

func compactPluginAuths(auths []*cliproxyauth.Auth) []*cliproxyauth.Auth {
	if len(auths) == 0 {
		return nil
	}
	out := auths[:0]
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if errWeight := cliproxyauth.ValidateAuthWeight(auth); errWeight != nil {
			continue
		}
		out = append(out, auth)
	}
	return out
}

func (s *FileTokenStore) idFor(path, baseDir string) string {
	id := path
	if baseDir != "" {
		if rel, errRel := filepath.Rel(baseDir, path); errRel == nil && rel != "" {
			id = rel
		}
	}
	// On Windows, normalize ID casing to avoid duplicate auth entries caused by case-insensitive paths.
	if runtime.GOOS == "windows" {
		id = strings.ToLower(id)
	}
	return id
}

// requireScopedExistingAuthFile rejects edits to stale records after an
// AuthDir change. Call under the same write lock used by Save.
func (s *FileTokenStore) requireScopedExistingAuthFile(path string) error {
	dir := strings.TrimSpace(s.baseDirSnapshot())
	if dir == "" {
		return cliproxyauth.ErrCredentialFileRemoved
	}
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		return cliproxyauth.ErrCredentialFileRemoved
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return cliproxyauth.ErrCredentialFileRemoved
	}
	rel, err := filepath.Rel(absoluteDir, absolutePath)
	if err != nil || !filepath.IsLocal(rel) {
		return cliproxyauth.ErrCredentialFileRemoved
	}
	root, err := os.OpenRoot(absoluteDir)
	if err != nil {
		return cliproxyauth.ErrCredentialFileRemoved
	}
	defer root.Close()
	info, err := root.Lstat(rel)
	if err != nil || !info.Mode().IsRegular() {
		return cliproxyauth.ErrCredentialFileRemoved
	}
	return nil
}

func (s *FileTokenStore) resolveAuthPath(auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("auth filestore: auth is nil")
	}
	if auth.Attributes != nil {
		if p := strings.TrimSpace(auth.Attributes["path"]); p != "" {
			return p, nil
		}
	}
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		if filepath.IsAbs(fileName) {
			return fileName, nil
		}
		if dir := s.baseDirSnapshot(); dir != "" {
			return filepath.Join(dir, fileName), nil
		}
		return fileName, nil
	}
	if auth.ID == "" {
		return "", fmt.Errorf("auth filestore: missing id")
	}
	if filepath.IsAbs(auth.ID) {
		return auth.ID, nil
	}
	dir := s.baseDirSnapshot()
	if dir == "" {
		return "", fmt.Errorf("auth filestore: directory not configured")
	}
	return filepath.Join(dir, auth.ID), nil
}

func (s *FileTokenStore) labelFor(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	if v, ok := metadata["label"].(string); ok && v != "" {
		return v
	}
	if v, ok := metadata["email"].(string); ok && v != "" {
		return v
	}
	if project, ok := metadata["project_id"].(string); ok && project != "" {
		return project
	}
	return ""
}

func (s *FileTokenStore) baseDirSnapshot() string {
	s.dirLock.RLock()
	defer s.dirLock.RUnlock()
	return s.baseDir
}

func extractAccessToken(metadata map[string]any) string {
	if at, ok := metadata["access_token"].(string); ok {
		if v := strings.TrimSpace(at); v != "" {
			return v
		}
	}
	if tokenMap, ok := metadata["token"].(map[string]any); ok {
		if at, ok := tokenMap["access_token"].(string); ok {
			if v := strings.TrimSpace(at); v != "" {
				return v
			}
		}
	}
	return ""
}

// jsonEqual compares two JSON blobs by parsing them into Go objects and deep comparing.
func jsonEqual(a, b []byte) bool {
	return cliproxyauth.EqualCredentialJSON(a, b)
}

func withAccountSettingsFileLock(path string, fn func() error) error {
	return codex.WithFileLock(path, fn)
}

func writeAccountSettingsWithHistory(path string, old, next []byte, guard *ordinaryCredentialMetadataGuard) error {
	return writeFileAtomicWithMetadata(path, next, 0600, guard, func() { snapshotAuthFileHistory(path) })
}
