package management

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var errUnsupportedAuthUploadStore = errors.New("configured credential store does not support scoped upload")

type authUploadTarget struct {
	root      *os.Root
	name      string
	path      string
	scope     os.FileInfo
	committed os.FileInfo
	selection authUploadSelection
}

type authUploadSelectionKey struct{}

type authUploadSelection struct {
	owner      *Handler
	generation uint64
	dir        string
}

func (h *Handler) withAuthUploadSelection(ctx context.Context) context.Context {
	if selection, ok := ctx.Value(authUploadSelectionKey{}).(authUploadSelection); ok && selection.owner == h {
		return ctx
	}
	h.mu.Lock()
	selection := authUploadSelection{h, h.fileOAuthGeneration, fileOAuthConfigDir(h.cfg)}
	h.mu.Unlock()
	return context.WithValue(ctx, authUploadSelectionKey{}, selection)
}

// The directory publication gate only covers local installation and short
// manager mutations. A remote request never blocks a config reload.
func (s authUploadSelection) begin() (func(), bool) {
	s.owner.authUploadScopeMu.RLock()
	if s.owner.fileOAuthGeneration != s.generation {
		s.owner.authUploadScopeMu.RUnlock()
		return nil, false
	}
	return s.owner.authUploadScopeMu.RUnlock, true
}

func (s authUploadSelection) check() error {
	release, ok := s.begin()
	if !ok {
		return coreauth.ErrAuthPublicationOutOfScope
	}
	release()
	return nil
}

func authUploadFailure(err error) (int, gin.H) {
	switch {
	case errors.Is(err, coreauth.ErrAuthUploadLocalPending):
		return http.StatusServiceUnavailable, gin.H{"code": "upload_local_pending", "error": coreauth.ErrAuthUploadLocalPending.Error()}
	case errors.Is(err, coreauth.ErrAuthUploadRemoteUnconfirmed):
		return http.StatusBadGateway, gin.H{"code": "upload_remote_unconfirmed", "error": coreauth.ErrAuthUploadRemoteUnconfirmed.Error()}
	case errors.Is(err, coreauth.ErrAuthUploadPayloadConflict):
		return http.StatusConflict, gin.H{"code": "upload_payload_conflict", "error": coreauth.ErrAuthUploadPayloadConflict.Error()}
	case errors.Is(err, coreauth.ErrAuthPublicationOutOfScope):
		return http.StatusConflict, gin.H{"code": "upload_scope_changed", "error": coreauth.ErrAuthPublicationOutOfScope.Error()}
	case errors.Is(err, errAuthFileMustBeJSON):
		return http.StatusBadRequest, gin.H{"error": "file must be .json"}
	case errors.Is(err, errInvalidAuthFile):
		return http.StatusBadRequest, gin.H{"error": err.Error()}
	case errors.Is(err, errUnsupportedAuthUploadStore):
		return http.StatusNotImplemented, gin.H{"error": err.Error()}
	default:
		return http.StatusInternalServerError, gin.H{"error": err.Error()}
	}
}

func openAuthUploadTarget(dir, name string) (*authUploadTarget, error) {
	if isUnsafeAuthFileName(name) || !filepath.IsLocal(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
		return nil, fmt.Errorf("%w: invalid credential file name", errInvalidAuthFile)
	}
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("credential directory is not configured")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve credential directory: %w", err)
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("open credential directory: %w", err)
	}
	scope, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	target := &authUploadTarget{root: root, name: name, path: filepath.Join(absolute, name), scope: scope}
	if err := target.validateDestination(); err != nil {
		_ = root.Close()
		return nil, err
	}
	return target, nil
}

func (t *authUploadTarget) validateDestination() error {
	info, err := t.root.Lstat(t.name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if info != nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%w: upload target must be a regular credential file", errInvalidAuthFile)
	}
	dir, err := os.Stat(filepath.Dir(t.path))
	if err != nil || !os.SameFile(dir, t.scope) {
		return errors.New("credential directory changed during upload; retry")
	}
	return nil
}

type authUploadStore interface {
	PersistAuthUpload(context.Context, string, []byte, func(commitRemote func() error) error) error
}

func (h *Handler) commitUploadedAuth(ctx context.Context, target *authUploadTarget, data []byte, lock bool) error {
	store := h.tokenStore
	if store == nil {
		store = sdkauth.GetTokenStore()
	}
	var backend authUploadStore
	switch typed := store.(type) {
	case *sdkauth.FileTokenStore:
		// The scoped commit below is the file store's durable write.
	case authUploadStore:
		backend = typed
	default:
		return errUnsupportedAuthUploadStore
	}

	commit := func(commitRemote func() error) error {
		if err := target.selection.check(); err != nil {
			return err
		}
		if err := target.validateDestination(); err != nil {
			return err
		}
		tmpName := ".upload-" + rand.Text()
		tmp, err := target.root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer target.root.Remove(tmpName)
		_, err = io.Copy(tmp, bytes.NewReader(data))
		if err == nil {
			err = tmp.Sync()
		}
		info, errStat := tmp.Stat()
		if err = errors.Join(err, errStat, tmp.Close()); err != nil {
			return err
		}
		if err := target.selection.check(); err != nil {
			return err
		}
		if commitRemote != nil {
			if err := commitRemote(); err != nil {
				return err
			}
		}
		release, eligible := target.selection.begin()
		if !eligible {
			return coreauth.ErrAuthPublicationOutOfScope
		}
		defer release()
		if err := target.validateDestination(); err != nil {
			return err
		}
		tmpInfo, err := target.root.Lstat(tmpName)
		if err != nil || !os.SameFile(info, tmpInfo) {
			return errors.New("staged credential changed during upload; retry")
		}

		// Rename operates on the directory entry and cannot follow a target
		// symlink introduced after validation.
		if err := target.root.Rename(tmpName, target.name); err != nil {
			return err
		}
		target.committed = info
		file, err := target.openCommitted()
		if err != nil {
			return err
		}
		_ = file.Close()
		return nil
	}
	install := func(commitRemote func() error) error {
		if lock {
			return codex.WithFileLockInRoot(target.root, target.name, func() error { return commit(commitRemote) })
		}
		return commit(commitRemote)
	}
	// Backing-store Save acquires store.mu first and then the credential lock.
	// Upload uses the same order; install runs inside the backing store's mutex.
	var err error
	if backend == nil {
		err = install(nil)
	} else {
		err = backend.PersistAuthUpload(ctx, target.path, data, install)
	}
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	return nil
}

func (t *authUploadTarget) openCommitted() (*os.File, error) {
	changed := errors.New("committed credential changed before publication; retry")
	if err := t.validateDestination(); err != nil {
		return nil, err
	}
	expected, err := t.root.Lstat(t.name)
	if err != nil || !os.SameFile(t.committed, expected) {
		return nil, changed
	}
	file, err := t.root.Open(t.name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !os.SameFile(t.committed, info) {
		_ = file.Close()
		return nil, changed
	}
	return file, nil
}

func (t *authUploadTarget) publicationContext(ctx context.Context) context.Context {
	ctx = coreauth.WithAuthPublicationFence(coreauth.WithSkipPersist(ctx), &coreauth.AuthPublicationFence{
		BeginPublication: t.selection.begin,
	})
	return coreauth.WithFileSnapshotReader(ctx, func(path string) ([]byte, error) {
		if filepath.Clean(path) != t.path {
			return nil, os.ErrPermission
		}
		file, err := t.openCommitted()
		if err != nil {
			return nil, err
		}
		defer file.Close()
		return io.ReadAll(file)
	})
}

func (t *authUploadTarget) matches(path string) bool {
	name := filepath.Base(path)
	if name != t.name && !(runtime.GOOS == "windows" && strings.EqualFold(name, t.name)) {
		// A case-insensitive filesystem can open the name under either case.
		other, err := t.root.Lstat(name)
		if err != nil || !os.SameFile(t.committed, other) {
			return false
		}
	}
	dir, err := os.Stat(filepath.Dir(path))
	return err == nil && os.SameFile(t.scope, dir)
}
