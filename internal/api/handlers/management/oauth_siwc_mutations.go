package management

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type siwcFileMutationKey struct{}

// UploadAuthFileV8 coordinates file replacement with explicit SIWC binding.
func (h *Handler) UploadAuthFileV8(c *gin.Context) {
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), siwcFileMutationKey{}, true))
	h.UploadAuthFile(c)
}

// DeleteAuthFileV8 coordinates file removal with explicit SIWC binding.
func (h *Handler) DeleteAuthFileV8(c *gin.Context) {
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), siwcFileMutationKey{}, true))
	h.DeleteAuthFile(c)
}

// beginSIWCFileMutation fences disk mutation through manager publication. Legacy
// callers deliberately retain their existing behavior. Never acquire flow.mu:
// exchange commits acquire flow.mu before this gate and then the session store.
func (h *Handler) beginSIWCFileMutation(ctx context.Context, path string) func() {
	if ctx == nil || ctx.Value(siwcFileMutationKey{}) != true {
		return func() {}
	}
	h.siwcCommitMu.Lock()
	h.invalidateSIWCFileMutationLocked(path)
	return h.siwcCommitMu.Unlock
}

// invalidateSIWCFileMutationLocked requires siwcCommitMu; it never takes flow.mu.
func (h *Handler) invalidateSIWCFileMutationLocked(path string) {
	oauthSessions.mu.Lock()
	now := time.Now()
	for state, session := range oauthSessions.sessions {
		flow := session.SIWC
		if flow == nil || flow.owner != h || session.Completed || session.Status != "" || !sameSIWCMutationPath(path, flow.path) {
			continue
		}
		// Even identical reuploads invalidate the old authorization intent.
		session.Status = "SIWC binding target changed; start a new binding"
		session.ExpiresAt = now.Add(oauthSessions.completedTTL)
		oauthSessions.sessions[state] = session
		flow.cancel()
	}
	oauthSessions.mu.Unlock()
}

type siwcFileMutationCommitKey struct{}

// beginSIWCUploadMutation serializes upload validation and publication without
// retiring the old intent until an actual file/credential commit succeeds.
func (h *Handler) beginSIWCUploadMutation(ctx context.Context, path string) (context.Context, func()) {
	if ctx == nil || ctx.Value(siwcFileMutationKey{}) != true {
		return ctx, func() {}
	}
	h.siwcCommitMu.Lock()
	committed := false
	markCommitted := func() {
		if !committed {
			committed = true
			h.invalidateSIWCFileMutationLocked(path)
		}
	}
	return context.WithValue(ctx, siwcFileMutationCommitKey{}, markCommitted), h.siwcCommitMu.Unlock
}

// commitSIWCUploadMutation is called synchronously before post-persist hooks.
func commitSIWCUploadMutation(ctx context.Context) {
	if ctx == nil {
		return
	}
	if commit, ok := ctx.Value(siwcFileMutationCommitKey{}).(func()); ok {
		commit()
	}
}

func sameSIWCMutationPath(a, b string) bool {
	// SameFile also handles case-insensitive filesystems on macOS and aliases.
	if ai, errA := os.Stat(a); errA == nil {
		if bi, errB := os.Stat(b); errB == nil && os.SameFile(ai, bi) {
			return true
		}
	}
	canonical := func(path string) string {
		if real, err := filepath.EvalSymlinks(path); err == nil {
			path = real
		} else if parent, errParent := filepath.EvalSymlinks(filepath.Dir(path)); errParent == nil {
			path = filepath.Join(parent, filepath.Base(path))
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		return filepath.Clean(path)
	}
	a, b = canonical(a), canonical(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
