// events.go implements fsnotify event handling for config and auth file changes.
// It normalizes paths, debounces noisy events, and triggers reload/update logic.
package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

func matchProvider(provider string, targets []string) (string, bool) {
	p := strings.ToLower(strings.TrimSpace(provider))
	for _, t := range targets {
		if strings.EqualFold(p, strings.TrimSpace(t)) {
			return p, true
		}
	}
	return p, false
}

func (w *Watcher) start(ctx context.Context) error {
	initialFile, initialErr := os.Stat(w.configPath)
	if errAddConfig := w.watcher.Add(w.configPath); errAddConfig != nil {
		log.Errorf("failed to watch config file %s: %v", w.configPath, errAddConfig)
		return errAddConfig
	}
	configParent := filepath.Dir(w.configPath)
	realConfig, resolveErr := filepath.EvalSymlinks(w.configPath)
	watchParent := func(parent string) error {
		err := w.watcher.Add(parent)
		if err == nil {
			return nil
		}
		if runtime.GOOS == "linux" && errors.Is(err, syscall.EACCES) && initialErr == nil &&
			initialFile.Mode().IsRegular() && resolveErr == nil {
			currentPath, pathErr := filepath.EvalSymlinks(w.configPath)
			currentFile, statErr := os.Stat(w.configPath)
			if pathErr == nil && statErr == nil && currentPath == realConfig && os.SameFile(initialFile, currentFile) {
				log.WithError(err).WithField("parent", parent).Warn("config parent watch unavailable; watching in-place config changes")
				return nil
			}
		}
		return fmt.Errorf("watch config parent %s: %w", parent, err)
	}
	// A rename drops the inode watch; directory watches persist across replacements.
	if err := watchParent(configParent); err != nil {
		return err
	}
	if resolveErr == nil && !w.sameNormalizedAuthPath(filepath.Dir(realConfig), configParent) {
		if err := watchParent(filepath.Dir(realConfig)); err != nil {
			return err
		}
	}

	w.watchMu.Lock()
	authDir := w.currentAuthDir()
	watchPath, err := physicalAuthWatchPath(authDir)
	if err == nil {
		err = w.watcher.Add(watchPath)
	}
	if err != nil {
		w.watchMu.Unlock()
		log.Errorf("failed to watch auth directory %s: %v", authDir, err)
		return fmt.Errorf("watch auth directory %s: %w", authDir, err)
	}
	w.started = true
	w.watchedAuthDir = authDir
	w.watchedAuthPath = watchPath
	w.watchMu.Unlock()
	log.Debugf("watching auth directory: %s", authDir)

	go w.processEvents(ctx)

	w.reloadClients(true, nil, false)
	return nil
}

func (w *Watcher) isConfigParent(path string) bool {
	if w.sameNormalizedAuthPath(path, filepath.Dir(w.configPath)) {
		return true
	}
	resolved, err := filepath.EvalSymlinks(w.configPath)
	return err == nil && w.sameNormalizedAuthPath(path, filepath.Dir(resolved))
}

func physicalAuthWatchPath(target string) (string, error) {
	realPath, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	return filepath.Abs(realPath)
}

// The caller holds watchMu. Keep the old subscription until publication.
func (w *Watcher) prepareAuthDirWatchLocked(target string) (string, error) {
	if !w.started || target == w.watchedAuthDir || w.sameNormalizedAuthPath(w.watchedAuthDir, target) {
		return "", nil
	}
	if w.stopped.Load() {
		return "", fmt.Errorf("auth watcher is stopped")
	}
	if _, err := os.ReadDir(target); err != nil {
		return "", fmt.Errorf("read auth directory %s: %w", target, err)
	}
	// A readable directory may still prohibit opening its children.
	if _, err := os.Stat(target + string(os.PathSeparator) + "."); err != nil {
		return "", fmt.Errorf("search auth directory %s: %w", target, err)
	}
	watchPath, err := physicalAuthWatchPath(target)
	if err != nil {
		return "", fmt.Errorf("resolve auth directory %s: %w", target, err)
	}
	// Different spellings of the same directory share the native subscription.
	if !w.sameNormalizedAuthPath(w.watchedAuthPath, watchPath) {
		if err := w.watcher.Add(watchPath); err != nil {
			return "", fmt.Errorf("watch auth directory %s: %w", target, err)
		}
	}
	return watchPath, nil
}

func (w *Watcher) finishAuthDirWatchLocked(target, watchPath string) {
	old := w.watchedAuthPath
	w.watchedAuthDir = target
	w.watchedAuthPath = watchPath
	if old != "" && !w.sameNormalizedAuthPath(old, watchPath) && !w.sameNormalizedAuthPath(old, w.configPath) && !w.isConfigParent(old) {
		if err := w.watcher.Remove(old); err != nil {
			log.Debugf("failed to remove previous auth directory watch %s: %v", old, err)
		}
	}
	log.Debugf("watching auth directory: %s", target)
}

func (w *Watcher) processEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			w.handleEvent(event)
		case errWatch, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			log.Errorf("file watcher error: %v", errWatch)
		}
	}
}

func (w *Watcher) handleEvent(event fsnotify.Event) {
	// Filter only relevant events: config file or auth-dir JSON files.
	configOps := fsnotify.Write | fsnotify.Create | fsnotify.Rename
	normalizedName := w.normalizeAuthPath(event.Name)
	normalizedConfigPath := w.normalizeAuthPath(w.configPath)
	isConfigPath := normalizedName == normalizedConfigPath
	if !isConfigPath && event.Op&configOps != 0 {
		if resolved, err := filepath.EvalSymlinks(w.configPath); err == nil {
			isConfigPath = normalizedName == w.normalizeAuthPath(resolved)
		}
	}
	authDir := w.currentAuthDir()
	isConfigEvent := isConfigPath && event.Op&configOps != 0
	authOps := fsnotify.Create | fsnotify.Write | fsnotify.Remove | fsnotify.Rename
	isAuthJSON := event.Op&authOps != 0 && strings.HasSuffix(normalizedName, ".json") && w.isPathInAuthDir(normalizedName, authDir)
	if !isConfigEvent && !isAuthJSON {
		// Ignore unrelated files (e.g., cookie snapshots *.cookie) and other noise.
		return
	}

	now := time.Now()
	log.Debugf("file system event detected: %s %s", event.Op.String(), event.Name)

	// Handle config file changes
	if isConfigEvent {
		log.Debugf("config file change details - operation: %s, timestamp: %s", event.Op.String(), now.Format("2006-01-02 15:04:05.000"))
		if event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
			// Best effort: a following Create event retries if the path was briefly absent.
			if err := w.watcher.Add(w.configPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Debugf("failed to reattach config watch %s: %v", w.configPath, err)
			}
		}
		w.scheduleConfigReload()
		return
	}

	// Handle auth directory changes incrementally (.json only)
	w.authRescanMu.Lock()
	defer w.authRescanMu.Unlock()
	authDir = w.currentAuthDir()
	if !w.isPathInAuthDir(normalizedName, authDir) {
		return
	}
	// Use the configured path for the same IDs and cache keys as full scans.
	event.Name = filepath.Join(authDir, filepath.Base(event.Name))
	normalizedName = w.normalizeAuthPath(event.Name)
	w.observeAuthFile(event.Name)

	if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		if w.shouldDebounceRemove(normalizedName, now) {
			log.Debugf("debouncing remove event for %s", filepath.Base(event.Name))
			return
		}
		// Atomic replace on some platforms may surface as Rename (or Remove) before the new file is ready.
		// Wait briefly; if the path exists again, treat as an update instead of removal.
		time.Sleep(replaceCheckDelay)
		if !w.isPathInAuthDir(normalizedName, w.currentAuthDir()) {
			return
		}
		statSucceeded := false
		if _, statErr := os.Stat(event.Name); statErr == nil {
			statSucceeded = true
		} else if w.isKnownAuthFile(event.Name) {
			// On macOS (and under disk I/O pressure), atomic replace (e.g. os.Rename from temp file)
			// can trigger a NOTE_DELETE before the target inode is fully visible. Retry briefly
			// before treating a known auth file as permanently removed.
			for attempt := 0; attempt < 3; attempt++ {
				time.Sleep(25 * time.Millisecond)
				if !w.isPathInAuthDir(normalizedName, w.currentAuthDir()) {
					return
				}
				if _, retryErr := os.Stat(event.Name); retryErr == nil {
					statSucceeded = true
					break
				}
			}
		}
		if statSucceeded {
			if unchanged, errSame := w.authFileUnchanged(event.Name); errSame == nil && unchanged {
				log.Debugf("auth file unchanged (hash match), skipping reload: %s", filepath.Base(event.Name))
				return
			}
			log.Infof("auth file changed (%s): %s, processing incrementally", event.Op.String(), filepath.Base(event.Name))
			w.addOrUpdateClientLocked(event.Name)
			return
		}
		if !w.isKnownAuthFile(event.Name) {
			log.Debugf("ignoring remove for unknown auth file: %s", filepath.Base(event.Name))
			return
		}
		log.Infof("auth file changed (%s): %s, processing incrementally", event.Op.String(), filepath.Base(event.Name))
		w.removeClientLocked(event.Name)
		return
	}
	if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
		if unchanged, errSame := w.authFileUnchanged(event.Name); errSame == nil && unchanged {
			log.Debugf("auth file unchanged (hash match), skipping reload: %s", filepath.Base(event.Name))
			return
		}
		log.Infof("auth file changed (%s): %s, processing incrementally", event.Op.String(), filepath.Base(event.Name))
		w.addOrUpdateClientLocked(event.Name)
	}
}

// observeAuthFile invalidates in-flight scans even if hash or content deduplication
// suppresses an update. It must not invalidate an otherwise valid queued auth event.
func (w *Watcher) observeAuthFile(path string) {
	normalized := w.normalizeAuthPath(path)
	if normalized == "" {
		return
	}
	w.clientsMutex.Lock()
	defer w.clientsMutex.Unlock()
	if !w.isPathInCurrentAuthDirLocked(normalized) {
		return
	}
	if w.fileObservations == nil {
		w.fileObservations = make(map[string]uint64)
	}
	w.fileObservations[normalized]++
	if w.activeAuthScans > 0 {
		// A reload may cache a hash before publishing its auth. Force the event
		// to parse it even if the scan finishes first, retaining the known path.
		if _, known := w.lastAuthHashes[normalized]; known {
			w.lastAuthHashes[normalized] = ""
		}
	}
}

func (w *Watcher) authFileUnchanged(path string) (bool, error) {
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return false, errRead
	}
	if len(data) == 0 {
		return false, nil
	}
	sum := sha256.Sum256(data)
	curHash := hex.EncodeToString(sum[:])

	normalized := w.normalizeAuthPath(path)
	w.clientsMutex.RLock()
	prevHash, ok := w.lastAuthHashes[normalized]
	w.clientsMutex.RUnlock()
	if ok && prevHash == curHash {
		return true, nil
	}
	return false, nil
}

func (w *Watcher) isKnownAuthFile(path string) bool {
	normalized := w.normalizeAuthPath(path)
	w.clientsMutex.RLock()
	defer w.clientsMutex.RUnlock()
	_, ok := w.lastAuthHashes[normalized]
	return ok
}

func (w *Watcher) normalizeAuthPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	cleaned := filepath.Clean(trimmed)
	if runtime.GOOS == "windows" {
		cleaned = strings.TrimPrefix(cleaned, `\\?\`)
		cleaned = strings.ToLower(cleaned)
	}
	return cleaned
}

func (w *Watcher) shouldDebounceRemove(normalizedPath string, now time.Time) bool {
	if normalizedPath == "" {
		return false
	}
	w.clientsMutex.Lock()
	if w.lastRemoveTimes == nil {
		w.lastRemoveTimes = make(map[string]time.Time)
	}
	if last, ok := w.lastRemoveTimes[normalizedPath]; ok {
		if now.Sub(last) < authRemoveDebounceWindow {
			w.clientsMutex.Unlock()
			return true
		}
	}
	w.lastRemoveTimes[normalizedPath] = now
	if len(w.lastRemoveTimes) > 128 {
		cutoff := now.Add(-2 * authRemoveDebounceWindow)
		for p, t := range w.lastRemoveTimes {
			if t.Before(cutoff) {
				delete(w.lastRemoveTimes, p)
			}
		}
	}
	w.clientsMutex.Unlock()
	return false
}
