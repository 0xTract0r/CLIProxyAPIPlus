package auth

import (
	"errors"
	"math"
	"sort"
	"time"
)

// Legacy windows have hourly precision. Their debt expires 24 hours after the
// END of the source hour, never earlier than an event represented by the bucket.
type pacingLegacyBucket struct {
	ObservedRequests int64 `json:"observed_requests"`
	ObservedTokens   int64 `json:"observed_tokens"`
	Requests         int64 `json:"requests"`
	Tokens           int64 `json:"tokens"`
	UnknownTokens    bool  `json:"unknown_tokens,omitempty"`
}

// WarmupPacingLegacyWindow contains the live old gate's counters, not a selector
// auth clone. Returned floors must be merged into that gate before new writes.
type WarmupPacingLegacyWindow struct {
	Requests []DailyWindowBucket
	Tokens   []DailyWindowBucket
}

// WarmupPacingLegacyEvent is one existing result/usage accounting event. Unknown
// usage is dated at observation, including late results from long-running calls.
type WarmupPacingLegacyEvent struct {
	Requests      int64
	Tokens        int64
	UnknownTokens bool
	CountOnly     bool
	Owner         *WarmupPacingAttempt
}

func pacingLegacyExpiry(hour int64) int64 { return (hour + 25) * int64(time.Hour) }

func validatePacingLegacy(s pacingState) error {
	if len(s.Legacy) > 26 {
		return errors.New("warmup pacing legacy window exceeds bounds")
	}
	for hour, b := range s.Legacy {
		if hour <= 0 || hour > s.Last/int64(time.Hour) || hour > math.MaxInt64/int64(time.Hour)-25 || b.Requests < 0 || b.Tokens < 0 || b.ObservedRequests < 0 || b.ObservedTokens < 0 {
			return errors.New("invalid warmup pacing legacy window")
		}
	}
	return nil
}

func pacingLegacyInput(window WarmupPacingLegacyWindow, now int64) (map[int64]pacingLegacyBucket, error) {
	if len(window.Requests) > 26 || len(window.Tokens) > 26 {
		return nil, errors.New("legacy input exceeds bounds")
	}
	out := make(map[int64]pacingLegacyBucket)
	for kind, buckets := range [][]DailyWindowBucket{window.Requests, window.Tokens} {
		seen := make(map[int64]bool)
		for _, bucket := range buckets {
			if bucket.Hour <= 0 || bucket.Hour > now/int64(time.Hour) || bucket.Count < 0 || seen[bucket.Hour] {
				return nil, errors.New("invalid legacy input")
			}
			seen[bucket.Hour] = true
			if pacingLegacyExpiry(bucket.Hour) <= now {
				continue
			}
			b := out[bucket.Hour]
			if kind == 0 {
				b.ObservedRequests = int64(bucket.Count)
			} else {
				b.ObservedTokens = int64(bucket.Count)
			}
			out[bucket.Hour] = b
		}
	}
	return out, nil
}

func pacingLegacyFloors(s pacingState) WarmupPacingLegacyWindow {
	var out WarmupPacingLegacyWindow
	hours := make([]int64, 0, len(s.Legacy))
	for hour := range s.Legacy {
		hours = append(hours, hour)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i] < hours[j] })
	for _, hour := range hours {
		b := s.Legacy[hour]
		if b.ObservedRequests > 0 {
			out.Requests = append(out.Requests, DailyWindowBucket{Hour: hour, Count: int(b.ObservedRequests)})
		}
		if b.ObservedTokens > 0 {
			out.Tokens = append(out.Tokens, DailyWindowBucket{Hour: hour, Count: int(b.ObservedTokens)})
		}
	}
	return out
}

// ReconcileLegacy imports only positive per-hour differences on startup or
// re-enable. A cross-file crash may conservatively charge an ambiguous result
// twice, but an older auth snapshot can never replace a higher durable watermark.
func (p *WarmupPacer) ReconcileLegacy(account string, window WarmupPacingLegacyWindow) (WarmupPacingLegacyWindow, error) {
	return p.legacyUpdate(account, window, nil)
}

// RecordLegacy mirrors a live accounting event once. Only an actual sent permit
// for this pacer/account proves its debit is already owned by the attempt ledger.
// The caller serializes old counter mutation through this durable update; normal
// Auth.Save must happen afterward and outside that coordination lock.
func (p *WarmupPacer) RecordLegacy(account string, window WarmupPacingLegacyWindow, event WarmupPacingLegacyEvent) (WarmupPacingLegacyWindow, error) {
	return p.legacyUpdate(account, window, &event)
}

func (p *WarmupPacer) legacyUpdate(account string, window WarmupPacingLegacyWindow, event *WarmupPacingLegacyEvent) (WarmupPacingLegacyWindow, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if account == "" {
		return WarmupPacingLegacyWindow{}, errors.New("warmup pacing account is empty")
	}
	a, err := p.load(pacingDigest(account))
	if err != nil {
		return WarmupPacingLegacyWindow{}, err
	}
	if a.state.Last == 0 {
		return WarmupPacingLegacyWindow{}, errors.New("warmup pacing policy is not configured")
	}
	s := clonePacingState(a.state)
	p.advance(a, &s, p.now().UnixNano())
	input, err := pacingLegacyInput(window, s.Last)
	if err != nil {
		return WarmupPacingLegacyWindow{}, err
	}
	for hour, current := range input {
		b := s.Legacy[hour]
		if event == nil {
			requests := max(0, current.ObservedRequests-b.ObservedRequests)
			tokens := max(0, current.ObservedTokens-b.ObservedTokens)
			b.Requests = pacingAdd(b.Requests, requests)
			b.Tokens = pacingAdd(b.Tokens, tokens)
			b.UnknownTokens = b.UnknownTokens || requests > 0 || tokens > 0
		}
		b.ObservedRequests = max(b.ObservedRequests, current.ObservedRequests)
		b.ObservedTokens = max(b.ObservedTokens, current.ObservedTokens)
		s.Legacy[hour] = b
	}
	if event != nil {
		if event.Requests < 0 || event.Tokens < 0 {
			return WarmupPacingLegacyWindow{}, errors.New("invalid legacy event")
		}
		owner := event.Owner
		owned := owner != nil && owner.pacer == p && owner.key == s.Key && owner.sent
		if !owned {
			hour := s.Last / int64(time.Hour)
			b := s.Legacy[hour]
			b.Requests = pacingAdd(b.Requests, event.Requests)
			b.Tokens = pacingAdd(b.Tokens, event.Tokens)
			b.UnknownTokens = b.UnknownTokens || event.UnknownTokens || (event.Requests > 0 && !event.CountOnly)
			s.Legacy[hour] = b
		}
	}
	if err := p.commit(a, s); err != nil {
		return WarmupPacingLegacyWindow{}, err
	}
	return pacingLegacyFloors(s), nil
}

// PacingLegacyWindow snapshots authoritative live counters after lazy seeding.
func (g *AccountConcurrencyGate) PacingLegacyWindow(authID string, seed WarmupPacingLegacyWindow) WarmupPacingLegacyWindow {
	g.mu.Lock()
	defer g.mu.Unlock()
	seedRollingLocked(g.daily, authID, seed.Requests)
	seedRollingLocked(g.tokens, authID, seed.Tokens)
	hour := g.currentHour()
	return WarmupPacingLegacyWindow{Requests: snapshotRollingLocked(g.daily, authID, hour), Tokens: snapshotRollingLocked(g.tokens, authID, hour)}
}

// RestorePacingLegacyFloor raises old counters after a stale auth-file recovery.
// It never replaces a newer ring slot or decreases a live counter.
func (g *AccountConcurrencyGate) RestorePacingLegacyFloor(authID string, floor WarmupPacingLegacyWindow) {
	g.mu.Lock()
	defer g.mu.Unlock()
	hour := g.currentHour()
	for kind, buckets := range [][]DailyWindowBucket{floor.Requests, floor.Tokens} {
		windows := g.daily
		if kind == 1 {
			windows = g.tokens
		}
		if windows[authID] == nil {
			windows[authID] = &rollingWindow{}
		}
		for _, bucket := range buckets {
			if bucket.Hour <= hour-24 || bucket.Hour > hour || bucket.Count <= 0 {
				continue
			}
			slot := &windows[authID].buckets[hourRingIndex(bucket.Hour)]
			if slot.hour < bucket.Hour {
				slot.hour = bucket.Hour
				slot.count = bucket.Count
			} else if slot.hour == bucket.Hour {
				slot.count = max(slot.count, bucket.Count)
			}
		}
	}
}
