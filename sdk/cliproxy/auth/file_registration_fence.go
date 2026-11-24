package auth

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
)

// FileRemovalInProgressError lets direct callers retry and watchers re-read
// the current source after all of the removal's credential gates have opened.
type FileRemovalInProgressError struct {
	manager *Manager
	retries map[[2]string]func()
}

func (*FileRemovalInProgressError) Error() string {
	return "source file removal in progress; retry after removal"
}

func (e *FileRemovalInProgressError) AfterRemoval(snapshot *Auth, retry func()) {
	e.manager.mu.Lock()
	if e.retries != nil {
		e.retries[[2]string{snapshot.ID, snapshot.fileSnapshotDigest}] = retry
		e.manager.mu.Unlock()
		return
	}
	e.manager.mu.Unlock()
	retry()
}

func authRemovalPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	p, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// A virtual source and its file path can identify the same file. Count it once.
func authRemovalPaths(a *Auth) []string {
	var paths []string
	for _, name := range []string{a.Attributes[AttributeVirtualSource], a.Attributes[AttributePath], a.FileSnapshotPath()} {
		if key := authRemovalPath(name); key != "" && !slices.Contains(paths, key) {
			paths = append(paths, key)
		}
	}
	return paths
}

var ErrStaleRegistrationSnapshot = errors.New("source file changed before registration; re-read current file")

// Reserve the source before reading a snapshot or persisting a new record.
// A concurrent deletion must enumerate after publication or retry.
func (m *Manager) beginFileRegistration(a *Auth) (func(), error) {
	paths := authRemovalPaths(a)
	if len(paths) == 0 {
		return func() {}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reserveFilePathsLocked(paths)
}

// reserveFilePathsLocked uses m.mu only for bookkeeping. Its release must be
// called after unlocking m.mu, before post-publication hooks run.
func (m *Manager) reserveFilePathsLocked(paths []string) (func(), error) {
	for _, path := range paths {
		if active := m.removingFiles[path]; active != nil {
			return nil, active
		}
	}
	if m.registeringFiles == nil {
		m.registeringFiles = make(map[string]int)
	}
	for _, path := range paths {
		m.registeringFiles[path]++
	}
	return sync.OnceFunc(func() {
		m.mu.Lock()
		for _, path := range paths {
			if m.registeringFiles[path]--; m.registeringFiles[path] == 0 {
				delete(m.registeringFiles, path)
			}
		}
		m.mu.Unlock()
	}), nil
}

var ErrFileRemovalBusy = errors.New("source file registration or removal in progress; retry deletion")

// BeginFileRemoval must precede member enumeration. The returned function
// runs any deferred re-reads after a successful or failed removal.
func (m *Manager) BeginFileRemoval(path string) (func(), error) {
	key := authRemovalPath(path)
	if key == "" {
		return nil, fmt.Errorf("invalid auth removal path")
	}
	m.mu.Lock()
	if m.removingFiles[key] != nil || m.registeringFiles[key] != 0 {
		m.mu.Unlock()
		return nil, ErrFileRemovalBusy
	}
	if m.removingFiles == nil {
		m.removingFiles = make(map[string]*FileRemovalInProgressError)
	}
	active := &FileRemovalInProgressError{manager: m, retries: make(map[[2]string]func())}
	m.removingFiles[key] = active
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		if m.removingFiles[key] != active {
			m.mu.Unlock()
			return
		}
		delete(m.removingFiles, key)
		retries := active.retries
		active.retries = nil
		m.mu.Unlock()
		for _, retry := range retries {
			retry()
		}
	}, nil
}

// changedFilePaths reserves both sources when an existing record moves. The
// unchanged-source case retains the existing per-record mutation gate.
func changedFilePaths(before, after *Auth) []string {
	oldPaths, newPaths := authRemovalPaths(before), authRemovalPaths(after)
	if slices.Equal(oldPaths, newPaths) {
		return nil
	}
	for _, path := range newPaths {
		if !slices.Contains(oldPaths, path) {
			oldPaths = append(oldPaths, path)
		}
	}
	return oldPaths
}
