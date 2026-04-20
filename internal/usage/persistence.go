package usage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const snapshotFileVersion = 2

type snapshotFile struct {
	Version int                `json:"version"`
	SavedAt time.Time          `json:"saved_at"`
	Usage   StatisticsSnapshot `json:"usage"`
}

// ConfigureDefaultPersistence configures snapshot persistence for the shared statistics store.
func ConfigureDefaultPersistence(path string) error {
	return defaultRequestStatistics.SetPersistencePath(path)
}

// SetPersistencePath configures the snapshot file path and eagerly restores a saved snapshot when present.
func (s *RequestStatistics) SetPersistencePath(path string) error {
	if s == nil {
		return nil
	}

	cleaned := strings.TrimSpace(path)
	if cleaned != "" {
		cleaned = filepath.Clean(cleaned)
		if !filepath.IsAbs(cleaned) {
			if abs, err := filepath.Abs(cleaned); err == nil {
				cleaned = abs
			}
		}
	}

	s.mu.Lock()
	s.persistPath = cleaned
	s.mu.Unlock()

	if cleaned == "" {
		return nil
	}

	return s.LoadFromPersistence()
}

// SaveToPersistence writes the current statistics snapshot to the configured file path.
func (s *RequestStatistics) SaveToPersistence() error {
	if s == nil {
		return nil
	}
	path := s.persistencePath()
	if path == "" {
		return nil
	}

	snapshot := s.Snapshot()
	payload := snapshotFile{
		Version: snapshotFileVersion,
		SavedAt: time.Now().UTC(),
		Usage:   snapshot,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// LoadFromPersistence restores a previously saved statistics snapshot into memory.
func (s *RequestStatistics) LoadFromPersistence() error {
	if s == nil {
		return nil
	}
	path := s.persistencePath()
	if path == "" {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	snapshot, err := decodeSnapshotFile(data)
	if err != nil {
		return err
	}
	s.mergeSnapshot(snapshot, false)
	return nil
}

func (s *RequestStatistics) persistencePath() string {
	if s == nil {
		return ""
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.persistPath
}

func (s *RequestStatistics) schedulePersistence() {
	if s == nil || s.persistencePath() == "" {
		return
	}

	s.persistDirty.Store(true)
	if !s.persistRunning.CompareAndSwap(false, true) {
		return
	}

	go s.persistLoop()
}

func (s *RequestStatistics) persistLoop() {
	for {
		s.persistDirty.Store(false)
		if err := s.SaveToPersistence(); err != nil {
			log.WithError(err).Warn("usage: failed to persist statistics snapshot")
		}
		if s.persistDirty.Load() {
			continue
		}

		s.persistRunning.Store(false)
		if !s.persistDirty.Load() {
			return
		}
		if !s.persistRunning.CompareAndSwap(false, true) {
			return
		}
	}
}

func decodeSnapshotFile(data []byte) (StatisticsSnapshot, error) {
	var wrapped struct {
		Version int             `json:"version"`
		Usage   json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && len(wrapped.Usage) > 0 && (wrapped.Version == 0 || (wrapped.Version >= 1 && wrapped.Version <= snapshotFileVersion)) {
		var snapshot StatisticsSnapshot
		if err := json.Unmarshal(wrapped.Usage, &snapshot); err != nil {
			return StatisticsSnapshot{}, err
		}
		return snapshot, nil
	}

	var snapshot StatisticsSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return StatisticsSnapshot{}, err
	}
	return snapshot, nil
}
