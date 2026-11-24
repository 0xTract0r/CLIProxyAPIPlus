package auth

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AccountConcurrencyGate counts in-flight work and rolling usage for caller
// supplied keys. It does not select credentials or register a runtime policy.
type AccountConcurrencyGate struct {
	// mu guards inflight and daily. One mutex is used deliberately: no counter
	// is ever read or written outside this lock, which makes the type race-free
	// by construction.
	mu sync.Mutex

	// inflight maps authID -> current number of in-flight requests. An entry is
	// deleted the moment its count returns to 0 (see Release), so the map is
	// bounded by the set of accounts with active traffic, not by history.
	inflight map[string]int

	// daily maps authID -> that account's rolling 24-hour REQUEST counter
	// (harden-account-scheduling-limiter P2). It replaced the earlier single
	// UTC-calendar-day counter, which both reset the whole day's budget on a
	// process restart (fail-open) and let an account spend up to a double budget
	// straddling a UTC midnight. A rolling 24h window keyed off hourly buckets
	// fixes both (design §2.1 A3). Entries are bounded by the credential set;
	// stale (>24h) buckets read as 0.
	daily map[string]*rollingWindow

	// tokens maps authID -> that account's rolling 24-hour BILLABLE-TOKEN counter
	// (P3 token hygiene). Same rolling-window mechanism as daily, but the unit is
	// billable tokens rather than requests. It is inert until the token-counting
	// sink (internal/usage, a separate slice) records into it; until then every
	// account's token count stays 0 and the token gate never fires.
	tokens map[string]*rollingWindow

	// now is the injected clock (default time.Now); it exists so tests can drive
	// the rolling-window hour rollover deterministically. It must be safe for
	// concurrent use in production (time.Now is).
	now func() time.Time
}

// dailyWindowBucketCount / dailyWindowBucketSeconds define the rolling-window
// resolution: 24 hourly buckets covering the trailing 24 hours. An hour index is
// unixSeconds/3600 (the Unix epoch is UTC midnight so hour 0 is stable), and a
// bucket's ring slot is hourIndex % 24, so each of the last 24 distinct hours
// maps to its own slot and a bucket tagged with an older hour reads as stale (0).
const (
	dailyWindowBucketCount   = 24
	dailyWindowBucketSeconds = 3600
)

// rollingWindow is one account's trailing-24h counter as a fixed ring of hourly
// buckets. Fixed-size (not a growing map) so memory is bounded per account, and
// the ring naturally prunes: a slot whose tagged hour is older than the current
// window is treated as empty on both write (reset in place) and read (skipped).
type rollingWindow struct {
	buckets [dailyWindowBucketCount]rollingWindowBucket
}

// rollingWindowBucket is one hour's tally within a rollingWindow. hour is the
// hour index this slot currently holds (0 = never written / 1970, always stale);
// count is that hour's tally (requests for the daily window, billable tokens for
// the token window).
type rollingWindowBucket struct {
	hour  int64
	count int
}

// DailyWindowBucket is the persistable (JSON-round-trippable) form of one
// rolling-window bucket, used to survive a process restart (P2 persistence):
// MarkResult writes the account's current window buckets into its auth.Metadata
// account_scheduling.daily_budget_window, and the selector/execution path seeds a
// cold in-memory gate from that persisted value on the first touch after restart.
type DailyWindowBucket struct {
	Hour  int64 `json:"h"`
	Count int   `json:"c"`
}

// AccountConcurrencyGateOption customizes a gate at construction.
type AccountConcurrencyGateOption func(*AccountConcurrencyGate)

// WithGateClock injects the clock the gate reads for UTC-day math. The supplied
// function MUST be safe to call from multiple goroutines concurrently. Passing
// nil is ignored and leaves the default (time.Now) in place.
func WithGateClock(now func() time.Time) AccountConcurrencyGateOption {
	return func(g *AccountConcurrencyGate) {
		if now != nil {
			g.now = now
		}
	}
}

// NewAccountConcurrencyGate builds an empty gate reading time.Now by default.
func NewAccountConcurrencyGate(opts ...AccountConcurrencyGateOption) *AccountConcurrencyGate {
	g := &AccountConcurrencyGate{
		inflight: make(map[string]int),
		daily:    make(map[string]*rollingWindow),
		tokens:   make(map[string]*rollingWindow),
		now:      time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(g)
		}
	}
	return g
}

// Acquire records one new in-flight request for authID and reports whether the
// account is still within its concurrency limit AFTER this acquire.
//
// It ALWAYS increments the live count (so every Acquire must be paired with
// exactly one Release, regardless of the returned bool) and returns:
//
//   - true when the post-increment count is <= limit, or when limit <= 0
//     ("no concurrency ceiling configured for this stage" -- the count is still
//     tracked so InFlight stays accurate, it just never reports over-limit), or
//     when authID == "" (nothing to key on: returns true, records nothing, and
//     the paired Release is a harmless no-op).
//   - false when the increment pushed the count past a positive limit.
//     The call is still paired with Release.
func (g *AccountConcurrencyGate) Acquire(authID string, limit int) bool {
	if authID == "" {
		return true
	}
	g.mu.Lock()
	count := g.inflight[authID] + 1
	g.inflight[authID] = count
	g.mu.Unlock()
	if limit <= 0 {
		return true
	}
	return count <= limit
}

// Release drops one in-flight request for authID. It floors at 0 (a spurious or
// double Release can never drive the count negative) and deletes the map entry
// once the count reaches 0, keeping the in-flight map bounded to accounts with
// active traffic. A "" authID or an unknown authID is a no-op.
func (g *AccountConcurrencyGate) Release(authID string) {
	if authID == "" {
		return
	}
	g.mu.Lock()
	if count, ok := g.inflight[authID]; ok {
		if count <= 1 {
			delete(g.inflight, authID)
		} else {
			g.inflight[authID] = count - 1
		}
	}
	g.mu.Unlock()
}

// InFlight returns the current number of in-flight requests recorded for
// authID (0 if none).
func (g *AccountConcurrencyGate) InFlight(authID string) int {
	if authID == "" {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inflight[authID]
}

// currentHour returns the current hour index (Unix seconds / 3600). Because the
// Unix epoch is UTC midnight and 3600 divides evenly, this lands exactly on hour
// boundaries; the rolling window sums the trailing 24 of these, so a request at
// 23:59:59Z and one at 00:00:00Z stay in the SAME 24h window (the "跨 UTC 午夜双倍"
// fix) and only drop off once they are a full 24h in the past.
func (g *AccountConcurrencyGate) currentHour() int64 {
	return g.now().Unix() / dailyWindowBucketSeconds
}

// hourRingIndex maps an hour index onto its rolling-window ring slot, normalized
// non-negative.
func hourRingIndex(hour int64) int {
	idx := hour % dailyWindowBucketCount
	if idx < 0 {
		idx += dailyWindowBucketCount
	}
	return int(idx)
}

// recordRollingLocked adds delta to authID's window (requests or tokens) in the
// current hour's bucket, resetting a stale slot first. Caller holds g.mu.
func recordRollingLocked(windows map[string]*rollingWindow, authID string, hour int64, delta int) {
	w := windows[authID]
	if w == nil {
		w = &rollingWindow{}
		windows[authID] = w
	}
	b := &w.buckets[hourRingIndex(hour)]
	if b.hour != hour {
		b.hour = hour
		b.count = 0
	}
	b.count += delta
}

// rollingCountLocked sums authID's window over the trailing 24h (buckets whose
// tagged hour is within (hour-24, hour]). A stale slot (older hour, or the 1970
// zero value) contributes 0. Caller holds g.mu.
func rollingCountLocked(windows map[string]*rollingWindow, authID string, hour int64) int {
	w := windows[authID]
	if w == nil {
		return 0
	}
	sum := 0
	for _, b := range w.buckets {
		if b.count != 0 && b.hour > hour-dailyWindowBucketCount && b.hour <= hour {
			sum += b.count
		}
	}
	return sum
}

// seedRollingLocked lazily populates authID's window from a persisted snapshot,
// but ONLY when the gate has no live entry for authID yet (a cold gate after a
// process restart). Once a live entry exists the in-memory window is
// authoritative and the (possibly stale) persisted seed is ignored -- exactly why
// seeding from a stale auth clone is harmless (design P2: "此刻持久值即真值,克隆
// 过期无害"). An empty seed never creates an entry. Caller holds g.mu.
func seedRollingLocked(windows map[string]*rollingWindow, authID string, seed []DailyWindowBucket) {
	if len(seed) == 0 {
		return
	}
	if _, ok := windows[authID]; ok {
		return
	}
	w := &rollingWindow{}
	for _, sb := range seed {
		if sb.Count == 0 {
			continue
		}
		slot := &w.buckets[hourRingIndex(sb.Hour)]
		// On a ring-slot collision keep the newer (larger) hour.
		if sb.Hour >= slot.hour {
			slot.hour = sb.Hour
			slot.count = sb.Count
		}
	}
	windows[authID] = w
}

// snapshotRollingLocked returns authID's non-stale, non-empty window buckets for
// persistence. Caller holds g.mu.
func snapshotRollingLocked(windows map[string]*rollingWindow, authID string, hour int64) []DailyWindowBucket {
	w := windows[authID]
	if w == nil {
		return nil
	}
	out := make([]DailyWindowBucket, 0, dailyWindowBucketCount)
	for _, b := range w.buckets {
		if b.count != 0 && b.hour > hour-dailyWindowBucketCount && b.hour <= hour {
			out = append(out, DailyWindowBucket{Hour: b.hour, Count: b.count})
		}
	}
	return out
}

// RecordRequest counts one real outbound request for authID against its rolling
// 24-hour request budget in the current hour bucket. A "" authID is a no-op.
func (g *AccountConcurrencyGate) RecordRequest(authID string) {
	if authID == "" {
		return
	}
	hour := g.currentHour()
	g.mu.Lock()
	recordRollingLocked(g.daily, authID, hour, 1)
	g.mu.Unlock()
}

// RecordRequestWindow records one request for authID and returns the account's
// updated rolling-window buckets for the caller to persist, lazily seeding a cold
// gate from `seed` first (restart re-seed). A "" authID is a no-op returning nil.
func (g *AccountConcurrencyGate) RecordRequestWindow(authID string, seed []DailyWindowBucket) []DailyWindowBucket {
	if authID == "" {
		return nil
	}
	hour := g.currentHour()
	g.mu.Lock()
	defer g.mu.Unlock()
	seedRollingLocked(g.daily, authID, seed)
	recordRollingLocked(g.daily, authID, hour, 1)
	return snapshotRollingLocked(g.daily, authID, hour)
}

// DailyCount returns how many requests authID has recorded over the trailing 24
// hours (0 if none). A "" authID returns 0.
func (g *AccountConcurrencyGate) DailyCount(authID string) int {
	if authID == "" {
		return 0
	}
	hour := g.currentHour()
	g.mu.Lock()
	defer g.mu.Unlock()
	return rollingCountLocked(g.daily, authID, hour)
}

// OverDailyBudget reports whether authID has met or exceeded a positive rolling
// 24h request budget. A non-positive budget means "unbounded" (mature accounts,
// design §5.1: quota headroom governs, not a fixed daily cap) and always returns
// false. A "" authID returns false.
func (g *AccountConcurrencyGate) OverDailyBudget(authID string, budget int) bool {
	if authID == "" || budget <= 0 {
		return false
	}
	hour := g.currentHour()
	g.mu.Lock()
	defer g.mu.Unlock()
	return rollingCountLocked(g.daily, authID, hour) >= budget
}

// OverDailyBudgetWindow is OverDailyBudget with a lazy restart re-seed: it
// populates a cold gate's window from `seed` (the persisted auth.Metadata value)
// before evaluating, so an account's already-spent budget survives a process
// restart instead of resetting to 0 (P2 fail-open fix).
func (g *AccountConcurrencyGate) OverDailyBudgetWindow(authID string, budget int, seed []DailyWindowBucket) bool {
	if authID == "" || budget <= 0 {
		return false
	}
	hour := g.currentHour()
	g.mu.Lock()
	defer g.mu.Unlock()
	seedRollingLocked(g.daily, authID, seed)
	return rollingCountLocked(g.daily, authID, hour) >= budget
}

// RecordTokens counts billable tokens for authID against its rolling 24h token
// budget (P3). A "" authID or non-positive tokens is a no-op. This is the write
// side the (not-yet-wired) internal/usage billable-token sink will call.
func (g *AccountConcurrencyGate) RecordTokens(authID string, tokens int) {
	if authID == "" || tokens <= 0 {
		return
	}
	hour := g.currentHour()
	g.mu.Lock()
	recordRollingLocked(g.tokens, authID, hour, tokens)
	g.mu.Unlock()
}

// RecordTokensWindow records billable tokens for authID and returns the updated
// token-window buckets for persistence, lazily seeding a cold gate from `seed`.
func (g *AccountConcurrencyGate) RecordTokensWindow(authID string, tokens int, seed []DailyWindowBucket) []DailyWindowBucket {
	if authID == "" {
		return nil
	}
	hour := g.currentHour()
	g.mu.Lock()
	defer g.mu.Unlock()
	seedRollingLocked(g.tokens, authID, seed)
	if tokens > 0 {
		recordRollingLocked(g.tokens, authID, hour, tokens)
	}
	return snapshotRollingLocked(g.tokens, authID, hour)
}

// TokenCount returns billable tokens recorded for authID over the trailing 24h.
func (g *AccountConcurrencyGate) TokenCount(authID string) int {
	if authID == "" {
		return 0
	}
	hour := g.currentHour()
	g.mu.Lock()
	defer g.mu.Unlock()
	return rollingCountLocked(g.tokens, authID, hour)
}

// OverTokenBudget reports whether authID has met or exceeded a positive rolling
// 24h billable-token budget. Non-positive budget = unbounded (mature accounts) =
// false. Lazily seeds a cold gate from `seed` (restart re-seed). A "" authID
// returns false.
func (g *AccountConcurrencyGate) OverTokenBudget(authID string, budget int, seed []DailyWindowBucket) bool {
	if authID == "" || budget <= 0 {
		return false
	}
	hour := g.currentHour()
	g.mu.Lock()
	defer g.mu.Unlock()
	seedRollingLocked(g.tokens, authID, seed)
	return rollingCountLocked(g.tokens, authID, hour) >= budget
}

// ---------------------------------------------------------------------------
// Rolling-window persistence (P2/P3): the counters are in-memory truth, but the
// current window is mirrored into auth.Metadata.account_scheduling so a process
// restart re-seeds an account's already-spent budget instead of losing it. These
// helpers translate between the in-memory []DailyWindowBucket and the JSON-safe
// metadata shape (a list of {h,c} objects, numbers round-tripping as float64).
// ---------------------------------------------------------------------------

const (
	// accountSchedulingDailyWindowKey / accountSchedulingTokenWindowKey are the
	// account_scheduling sub-keys the rolling REQUEST / TOKEN windows persist
	// under. They sit next to rate_scale / first_production_at in the same
	// top-level account_scheduling object, so Auth.Clone carries them through a
	// quota refresh (account_scheduling_metadata.go).
	accountSchedulingDailyWindowKey = "daily_budget_window"
	accountSchedulingTokenWindowKey = "token_budget_window"
)

// dailyWindowToMetadata renders window buckets as the JSON-safe list stored under
// account_scheduling. A nil/empty window renders as an empty list so a spent
// window that fully aged out clears the persisted value rather than lingering.
func dailyWindowToMetadata(buckets []DailyWindowBucket) []any {
	out := make([]any, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, map[string]any{"h": b.Hour, "c": b.Count})
	}
	return out
}

// readDailyWindowBuckets parses the persisted rolling-window list stored under
// account_scheduling[key], tolerating the numeric shapes a JSON round-trip yields
// (float64 / json.Number) as well as in-memory int fixtures. Returns nil when
// absent or malformed.
func readDailyWindowBuckets(meta map[string]any, key string) []DailyWindowBucket {
	raw, ok := accountSchedulingRawValue(meta, key)
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]DailyWindowBucket, 0, len(list))
	for _, item := range list {
		obj, ok := metadataObject(item)
		if !ok {
			continue
		}
		hour, okHour := metadataInt64(obj["h"])
		count, okCount := metadataInt64(obj["c"])
		if !okHour || !okCount {
			continue
		}
		out = append(out, DailyWindowBucket{Hour: hour, Count: int(count)})
	}
	return out
}

// metadataInt64 coerces a metadata numeric value (float64 / json.Number / int /
// int64 / numeric string) into an int64, mirroring parseRateScaleValue's shape
// tolerance for persisted-and-reloaded auth.Metadata.
func metadataInt64(raw any) (int64, bool) {
	switch v := raw.(type) {
	case float64:
		return int64(v), true
	case float32:
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, true
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Manager wiring: the execution/completion path drives the gate the active
// AdaptiveSelector owns, so the selector's Pick-time avoidance and the
// execution path's acquire/release/record all share one live count.
// ---------------------------------------------------------------------------
