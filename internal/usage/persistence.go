package usage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	log "github.com/sirupsen/logrus"
)

const snapshotFileVersion = 2

// ErrUsageSnapshotChanged signals that a newer writer changed the history
// since this store loaded or last committed it.
var ErrUsageSnapshotChanged = errors.New("usage history changed since it was loaded or last saved")

// Serialize the compare and rename across stores in this process.
var usageSnapshotCommitMu sync.Mutex

// A bounded provenance cache. Eviction fails closed for stale writers.
var usageSnapshotLineages = make(map[string]snapshotLineage)
var nextUsageSnapshotLineage uint64

type snapshotLineage struct {
	id           uint64
	base, latest snapshotRevision
}

type snapshotRevision struct {
	exists bool
	digest [sha256.Size]byte
}

func readUsageSnapshotRevision(path string) ([]byte, snapshotRevision, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, snapshotRevision{}, nil
	}
	if err != nil {
		return nil, snapshotRevision{}, err
	}
	return data, snapshotRevision{exists: true, digest: sha256.Sum256(data)}, nil
}

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

	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	if err := s.loadFromPath(cleaned); err != nil {
		return err
	}
	s.mu.Lock()
	s.persistPath = cleaned
	s.mu.Unlock()
	return nil
}

// SaveToPersistence writes the current statistics snapshot to the configured file path.
func (s *RequestStatistics) SaveToPersistence() error {
	if s == nil {
		return nil
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	return s.saveToPath(s.persistencePath())
}

// ErrUsageImportInvalid marks input rejected by the complete history validator.
var ErrUsageImportInvalid = errors.New("invalid usage import")

// ImportAndSave merges an import and acknowledges it only after a configured
// history file is committed. A pathless instance retains its in-memory contract.
func (s *RequestStatistics) ImportAndSave(snapshot StatisticsSnapshot) (MergeResult, error) {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	var result MergeResult
	err := s.commitUsageSnapshot(s.persistencePath(), func(candidate *RequestStatistics) error {
		var err error
		result, err = candidate.mergeSnapshotChecked(snapshot, false, false)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrUsageImportInvalid, err)
		}
		return nil
	})
	if err != nil {
		return MergeResult{}, err
	}
	return result, nil
}

func (s *RequestStatistics) saveToPath(path string) error {
	return s.commitUsageSnapshot(path, nil)
}

func (s *RequestStatistics) commitUsageSnapshot(path string, apply func(*RequestStatistics) error) error {
	if path == "" {
		if apply != nil {
			return apply(s)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Lock a stable sidecar rather than the history inode, which is replaced
	// by rename. All cooperating writers must leave the sidecar in place.
	lock := flock.New(path+".lock", flock.SetFlag(os.O_CREATE|os.O_RDWR), flock.SetPermissions(0o600))
	defer lock.Close()
	lockCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	locked, err := lock.TryLockContext(lockCtx, 25*time.Millisecond)
	if err != nil {
		return fmt.Errorf("lock usage history: %w", err)
	}
	if !locked {
		return errors.New("usage history lock unavailable")
	}
	usageSnapshotCommitMu.Lock()
	defer usageSnapshotCommitMu.Unlock()
	disk, current, err := readUsageSnapshotRevision(path)
	if err != nil {
		return err
	}
	lineage, tracked := usageSnapshotLineages[path]
	var latest *StatisticsSnapshot
	if current != s.persistRevision {
		if !tracked || !current.exists || current != lineage.latest ||
			(s.persistLineage != lineage.id && s.persistRevision != lineage.base) {
			return ErrUsageSnapshotChanged
		}
		restored, err := decodeSnapshotFile(disk)
		if err != nil {
			return err
		}
		latest = &restored
	}
	candidate := s
	if latest != nil || apply != nil {
		// Keep any concurrent live Record ordered before or after this commit.
		s.mu.Lock()
		defer s.mu.Unlock()
		candidate = &RequestStatistics{
			statisticsData:       s.cloneStatisticsDataLocked(),
			knownUsageKeyBuckets: s.knownUsageKeyBuckets,
			catalog:              s.catalog,
		}
		if latest != nil {
			if _, err := candidate.mergeSnapshotChecked(*latest, false, true); err != nil {
				return err
			}
		}
		if apply != nil {
			if err := apply(candidate); err != nil {
				return err
			}
		}
	}
	payload := snapshotFile{
		Version: snapshotFileVersion,
		SavedAt: time.Now().UTC(),
		Usage:   candidate.Snapshot(),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := writeUsageSnapshot(path, data); err != nil {
		return err
	}
	if candidate != s {
		s.statisticsData = candidate.statisticsData
	}
	if !tracked || current != lineage.latest {
		nextUsageSnapshotLineage++
		lineage = snapshotLineage{id: nextUsageSnapshotLineage, base: current}
	}
	lineage.latest = snapshotRevision{exists: true, digest: sha256.Sum256(data)}
	if len(usageSnapshotLineages) >= 256 && !tracked {
		clear(usageSnapshotLineages)
	}
	usageSnapshotLineages[path] = lineage
	s.persistLineage, s.persistRevision = lineage.id, lineage.latest
	return nil
}

func writeUsageSnapshot(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// LoadFromPersistence restores a previously saved statistics snapshot into memory.
func (s *RequestStatistics) LoadFromPersistence() error {
	if s == nil {
		return nil
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	return s.loadFromPath(s.persistencePath())
}

func (s *RequestStatistics) loadFromPath(path string) error {
	usageSnapshotCommitMu.Lock()
	defer usageSnapshotCommitMu.Unlock()
	if path == "" {
		s.persistLineage = 0
		s.persistRevision = snapshotRevision{}
		return nil
	}
	data, revision, err := readUsageSnapshotRevision(path)
	if err != nil {
		return err
	}
	if !revision.exists {
		s.persistLineage = 0
		s.persistRevision = revision
		return nil
	}

	snapshot, err := decodeSnapshotFile(data)
	if err != nil {
		return err
	}
	if _, err := s.mergeSnapshotChecked(snapshot, false, true); err != nil {
		return err
	}
	s.persistRevision, s.persistLineage = revision, 0
	if lineage, ok := usageSnapshotLineages[path]; ok && lineage.latest == revision {
		s.persistLineage = lineage.id
	}
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
	var wrapped *struct {
		Version   *int            `json:"version"`
		Usage     json.RawMessage `json:"usage"`
		HasMore   bool            `json:"has_more"`
		NextSince string          `json:"next_since"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return StatisticsSnapshot{}, err
	}
	if wrapped == nil {
		return StatisticsSnapshot{}, errors.New("empty usage snapshot")
	}
	if wrapped.HasMore || wrapped.NextSince != "" {
		return StatisticsSnapshot{}, errors.New("a usage export page cannot be restored as a complete snapshot")
	}
	if wrapped.Version != nil || wrapped.Usage != nil {
		if wrapped.Version != nil && (*wrapped.Version < 0 || *wrapped.Version > snapshotFileVersion) {
			return StatisticsSnapshot{}, fmt.Errorf("unsupported usage snapshot version %d", *wrapped.Version)
		}
		if wrapped.Usage == nil || string(wrapped.Usage) == "null" {
			return StatisticsSnapshot{}, errors.New("missing usage snapshot contents")
		}
		data = wrapped.Usage
	}

	// The writer and historical exports always include apis, even when empty.
	// A bare JSON object without it cannot be distinguished from unrelated data.
	var payload struct {
		StatisticsSnapshot
		APIs json.RawMessage `json:"apis"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return StatisticsSnapshot{}, err
	}
	if payload.APIs == nil {
		return StatisticsSnapshot{}, errors.New("usage snapshot is missing its API records")
	}
	snapshot := payload.StatisticsSnapshot
	if err := json.Unmarshal(payload.APIs, &snapshot.APIs); err != nil {
		return StatisticsSnapshot{}, err
	}
	if err := validateUsageSnapshotForRestore(snapshot); err != nil {
		return StatisticsSnapshot{}, err
	}
	return snapshot, nil
}

type restoredUsageTotals struct {
	requests, failed, tokens, billable int64
}

func (totals *restoredUsageTotals) add(bucket restoredUsageTotals) error {
	if bucket.tokens > math.MaxInt64-totals.tokens || bucket.billable > math.MaxInt64-totals.billable {
		return errors.New("usage snapshot token totals overflow")
	}
	totals.requests += bucket.requests
	totals.failed += bucket.failed
	totals.tokens += bucket.tokens
	totals.billable += bucket.billable
	return nil
}

func (totals restoredUsageTotals) agreesWithSummary(requests, tokens, billable, unpriced, unfinalized int64, cost float64) bool {
	return requests >= 0 && requests <= totals.requests &&
		tokens >= 0 && tokens <= totals.tokens &&
		billable >= 0 && billable <= totals.billable &&
		unpriced >= 0 && unpriced <= totals.requests &&
		unfinalized >= 0 && unfinalized <= totals.requests &&
		cost >= 0 && (cost == 0 || totals.requests > 0)
}

func validateUsageSnapshotForRestore(snapshot StatisticsSnapshot) error {
	var totals restoredUsageTotals
	for apiName, api := range snapshot.APIs {
		if strings.TrimSpace(apiName) == "" || api.Models == nil {
			return errors.New("usage snapshot has an invalid API records group")
		}
		var apiTotals restoredUsageTotals
		for _, model := range api.Models {
			var modelTotals restoredUsageTotals
			for _, detail := range model.Details {
				if detail.Timestamp.IsZero() {
					return errors.New("usage snapshot contains a request without a timestamp")
				}
				tokens, err := checkedUsageTokens(detail.Tokens)
				if err != nil {
					return err
				}
				if err := checkUsageDetail(detail); err != nil {
					return err
				}
				counts := restoredUsageTotals{requests: 1, tokens: tokens.TotalTokens, billable: tokens.BillableTokens}
				if detail.Failed {
					counts.failed = 1
				}
				if err := modelTotals.add(counts); err != nil {
					return err
				}
			}
			if !modelTotals.agreesWithSummary(model.TotalRequests, model.TotalTokens, model.TotalBillableTokens,
				model.UnpricedRequestCount, model.UnfinalizedRequestCount, model.TotalCostUSD) {
				return errors.New("usage snapshot model summary exceeds recoverable request details")
			}
			if err := apiTotals.add(modelTotals); err != nil {
				return err
			}
		}
		if !apiTotals.agreesWithSummary(api.TotalRequests, api.TotalTokens, api.TotalBillableTokens,
			api.UnpricedRequestCount, api.UnfinalizedRequestCount, api.TotalCostUSD) {
			return errors.New("usage snapshot API summary exceeds recoverable request details")
		}
		if err := totals.add(apiTotals); err != nil {
			return err
		}
	}
	if !totals.agreesWithSummary(snapshot.TotalRequests, snapshot.TotalTokens, snapshot.TotalBillableTokens,
		snapshot.UnpricedRequestCount, snapshot.UnfinalizedRequestCount, snapshot.TotalCostUSD) ||
		snapshot.SuccessCount < 0 || snapshot.SuccessCount > totals.requests-totals.failed ||
		snapshot.FailureCount < 0 || snapshot.FailureCount > totals.failed ||
		snapshot.UnpricedModelCount < 0 || snapshot.UnpricedModelCount > totals.requests ||
		snapshot.UnfinalizedModelCount < 0 || snapshot.UnfinalizedModelCount > totals.requests ||
		!usageBucketsFit(snapshot.RequestsByDay, totals.requests) ||
		!usageBucketsFit(snapshot.RequestsByHour, totals.requests) ||
		!usageBucketsFit(snapshot.TokensByDay, totals.tokens) ||
		!usageBucketsFit(snapshot.TokensByHour, totals.tokens) {
		return errors.New("usage snapshot summary exceeds recoverable request details")
	}
	if totals.requests == 0 {
		for _, costs := range []map[string]float64{snapshot.CostByDay, snapshot.CostByHour} {
			for _, cost := range costs {
				if cost != 0 {
					return errors.New("usage snapshot has costs without request details")
				}
			}
		}
	}
	return nil
}

func usageBucketsFit(buckets map[string]int64, total int64) bool {
	for _, count := range buckets {
		if count < 0 || count > total {
			return false
		}
		total -= count
	}
	return true
}
