package cliproxy

import (
	"path/filepath"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func (s *Service) persistedFilePublicationFence(auth *coreauth.Auth) *coreauth.AuthPublicationFence {
	if s.localStoreRoot == nil || auth.AuthSourceKind() != coreauth.AuthSourceFile || coreauth.IsPluginVirtualAuth(auth) {
		return nil
	}
	if s.pluginHost != nil && s.pluginHost.HasAuthProvider(auth.Provider) {
		return nil
	}
	path := auth.FileSnapshotPath()
	if path == "" {
		path = auth.Attributes[coreauth.AttributePath]
	}
	if path == "" {
		return nil
	}

	s.cfgMu.RLock()
	generation := s.authDirGeneration
	var authDir string
	if s.cfg != nil {
		authDir = s.cfg.AuthDir
	}
	s.cfgMu.RUnlock()
	eligible := isInAuthDirectory(authDir, path)
	return &coreauth.AuthPublicationFence{BeginPublication: func() (func(), bool) {
		if !eligible {
			return nil, false
		}
		s.cfgMu.RLock()
		if generation != s.authDirGeneration {
			s.cfgMu.RUnlock()
			return nil, false
		}
		return s.cfgMu.RUnlock, true
	}}
}

func isInAuthDirectory(dir, path string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	dir, dirErr := filepath.Abs(dir)
	path, pathErr := filepath.Abs(path)
	if dirErr != nil || pathErr != nil {
		return false
	}
	dir, dirErr = filepath.EvalSymlinks(dir)
	path, pathErr = filepath.EvalSymlinks(path)
	if dirErr != nil || pathErr != nil {
		return false
	}
	relative, err := filepath.Rel(dir, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
