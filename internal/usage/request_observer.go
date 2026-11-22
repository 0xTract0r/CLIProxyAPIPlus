package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// SetRecordingEnabled affects new events without changing historical data.
func (s *RequestStatistics) SetRecordingEnabled(enabled bool) {
	if s != nil {
		s.observerMu.Lock()
		s.recordingDisabled = !enabled
		s.observerMu.Unlock()
	}
}

// Close waits for accepted events and detaches the history file, so a delayed
// callback from this instance cannot overwrite a replacement server's snapshot.
func (s *RequestStatistics) Close() error {
	if s == nil {
		return nil
	}
	s.observerMu.Lock()
	s.observerClosed = true
	s.observerMu.Unlock()
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.Lock()
	path := s.persistPath
	s.persistPath = ""
	s.mu.Unlock()
	return s.saveToPath(path)
}

// HandleUsage receives only events attached to this server's request context.
// Authenticate the management reader separately; no plaintext client key,
// HTTP headers, provider credential or request body belongs in the snapshot.
func (s *RequestStatistics) HandleUsage(_ context.Context, record coreusage.Record) {
	if s == nil {
		return
	}
	s.observerMu.RLock()
	defer s.observerMu.RUnlock()
	if s.observerClosed || s.recordingDisabled {
		return
	}
	bucket := record.Provider
	if key := strings.TrimSpace(record.APIKey); key != "" {
		bucket = usageKeyBucket(key)
	}
	s.Record(logging.WithRequestID(context.Background(), record.RequestID), coreusage.Record{
		APIKey: bucket, Provider: record.Provider, Model: record.Model, Alias: record.Alias,
		RequestedAt: record.RequestedAt, Failed: record.Failed, Detail: record.Detail,
		ServiceTier: record.ServiceTier, ReasoningEffort: record.ReasoningEffort,
		Latency: record.Latency, TTFT: record.TTFT,
	})
}

const usageKeyBucketPrefix = "h:"

func usageKeyBucket(key string) string {
	sum := sha256.Sum256([]byte(key))
	return usageKeyBucketPrefix + hex.EncodeToString(sum[:])
}

func usageKeyHash(bucket string) string {
	digest, ok := strings.CutPrefix(bucket, usageKeyBucketPrefix)
	if !ok || len(digest) != 64 || strings.ToLower(digest) != digest {
		return ""
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return ""
	}
	return digest
}

// SetKnownUsageAPIKeys sets the exact configured inbound keys used to recognize
// older bucket labels. Store only digests of the configured keys.
func (s *RequestStatistics) SetKnownUsageAPIKeys(keys []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.knownUsageKeyBuckets == nil {
		s.knownUsageKeyBuckets = make(map[string]struct{}, len(keys))
	}
	rebuild := false
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		bucket := usageKeyBucket(key)
		if _, known := s.knownUsageKeyBuckets[bucket]; known {
			continue
		}
		// Keep knowledge of committed keys until this instance exits. An
		// older import can still refer to a key removed by a later reload.
		s.knownUsageKeyBuckets[bucket] = struct{}{}
		hash := usageKeyHash(bucket)
		for _, label := range []string{key, "sha256:" + hash} {
			if label == bucket {
				continue
			}
			legacy := s.apis[label]
			if legacy == nil {
				continue
			}
			delete(s.apis, label)
			for _, model := range legacy.Models {
				for i := range model.Details {
					model.Details[i].APIKeyHash, model.Details[i].Source = hash, bucket
				}
			}
			current := s.apis[bucket]
			if current == nil {
				s.apis[bucket] = legacy
				continue
			}
			mergeKnownUsageBucket(bucket, current, legacy)
			rebuild = true
		}
	}
	if rebuild {
		// Aggregates change only when there really was a canonical/legacy
		// collision. Ordinary reloads, adds and removals never scan history.
		s.recalculatePricingLocked()
	}
}

func mergeKnownUsageBucket(bucket string, target, legacy *apiStats) {
	for name, old := range legacy.Models {
		current := target.Models[name]
		if current == nil {
			target.Models[name] = old
			continue
		}
		counts := make(map[string]int, len(current.Details))
		for _, detail := range current.Details {
			counts[dedupKey(bucket, name, detail)]++
		}
		for _, detail := range old.Details {
			key := dedupKey(bucket, name, detail)
			if counts[key] > 0 {
				counts[key]--
				continue
			}
			current.Details = append(current.Details, detail)
		}
	}
}

func (s *RequestStatistics) normalizeUsageBucketLocked(label string) string {
	if usageKeyHash(label) != "" {
		return label
	}
	if digest, ok := strings.CutPrefix(label, "sha256:"); ok {
		bucket := usageKeyBucketPrefix + digest
		if _, known := s.knownUsageKeyBuckets[bucket]; known {
			return bucket
		}
	}
	bucket := usageKeyBucket(label)
	if _, known := s.knownUsageKeyBuckets[bucket]; known {
		return bucket
	}
	return label
}
