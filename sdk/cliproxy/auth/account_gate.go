package auth

import (
	"net/http"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// AccountConcurrencyGate is the per-account in-flight concurrency semaphore and
// per-account UTC-daily request budget counter for the adaptive
// account-scheduling change
// (openspec/changes/add-adaptive-account-scheduling, design.md §5.1 / §6.2).
//
// It closes the two gaps the earlier warm-up wiring left open:
//
//   - Concurrency: config.AccountWarmupStage.ConcurrencyLimit /
//     config.AccountMatureLimitsConfig.ConcurrencyLimit describe how many
//     requests one account may have IN FLIGHT at once, but nothing counted
//     real in-flight requests. This type maintains that live count so the
//     adaptive selector can steer away from an account already at its ceiling
//     and the execution path can hold/release a slot for a request's lifetime.
//
//   - Daily budget: config.AccountWarmupStage.DailyBudget is design §5.1's
//     PRIMARY warm-up throttle ("第1周 ≤200/日 ..."), yet no request was ever
//     counted against it. This type keeps one request-per-UTC-day counter per
//     account so the selector can skip a warming account that has spent its
//     day's budget (mature accounts have DailyBudget 0 = unbounded).
//
// Design intent this type deliberately encodes:
//
//   - Per-account, never a global pool (design D2): every authID has its own
//     independent in-flight count and daily counter; one busy account never
//     constrains another.
//
//   - In-memory, restart starts conservative, no persistence (design §6.2):
//     on a process restart every in-flight count is 0 and every daily counter
//     is empty. Losing the daily count on restart errs toward LESS throttling
//     (an account could serve slightly more than its day's budget across a
//     restart) which is a bounded, self-correcting direction, exactly why §6.2
//     classifies this as "safe to lose on restart, no DB". Losing in-flight
//     counts on restart is likewise safe: in-flight requests belonging to the
//     dead process are gone, so a fresh 0 is correct.
//
//   - Soft ceiling, never a hard denier (task brief): Acquire ALWAYS records
//     the slot (so Release always pairs and the live count stays accurate even
//     when two goroutines race the same last slot -- the accepted "偶发 +1
//     瞬时越界") and only REPORTS whether the post-acquire count is within the
//     limit. The caller decides what to do with an over-limit report (the
//     non-stream execution path fails over to another credential before it has
//     sent anything; the stream path keeps the slot because the request is
//     already out). This gate never manufactures a failure the upstream did
//     not send.
//
// All exported methods are safe for concurrent use; a single mutex serializes
// every access to both maps (correctness/race-freedom is the priority here, per
// the design's concurrency-critical framing).
type AccountConcurrencyGate struct {
	// mu guards inflight and daily. One mutex is used deliberately: no counter
	// is ever read or written outside this lock, which makes the type race-free
	// by construction.
	mu sync.Mutex

	// inflight maps authID -> current number of in-flight requests. An entry is
	// deleted the moment its count returns to 0 (see Release), so the map is
	// bounded by the set of accounts with active traffic, not by history.
	inflight map[string]int

	// daily maps authID -> that account's request count for a single UTC day.
	// A stale-day entry reads as 0 (see dailyCountLocked) and is reset in place
	// on the next RecordRequest, so the map is bounded by the credential set.
	daily map[string]*dailyCounter

	// now is the injected clock (default time.Now); it exists so tests can drive
	// the UTC-day rollover deterministically. It must be safe for concurrent use
	// in production (time.Now is).
	now func() time.Time
}

// dailyCounter is one account's request count scoped to a single UTC day. day
// is the UTC day index (Unix seconds / 86400); count is that day's requests.
type dailyCounter struct {
	day   int64
	count int
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
		daily:    make(map[string]*dailyCounter),
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
//   - false when the increment pushed the count past a positive limit. The
//     caller may then Release and fail over to another account (it has not sent
//     anything yet), or -- when the request is already in flight (streaming) --
//     keep the slot and accept the transient overage.
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
// authID (0 if none). It is a plain read used by the selector to steer away
// from an account already at its ceiling; it does not itself gate.
func (g *AccountConcurrencyGate) InFlight(authID string) int {
	if authID == "" {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inflight[authID]
}

// currentDay returns the UTC day index (Unix seconds / 86400). Because the Unix
// epoch is itself UTC midnight and 86400 divides evenly, this lands exactly on
// UTC calendar-day boundaries, so a request at 23:59:59Z and one at 00:00:00Z
// fall in different buckets -- the design §5.1 "每天" reset.
func (g *AccountConcurrencyGate) currentDay() int64 {
	return g.now().UTC().Unix() / 86400
}

// RecordRequest counts one real outbound request for authID against today's
// (UTC) budget, resetting the account's counter to this day first if its last
// recorded request was on an earlier day. Call it exactly where a request is
// actually sent upstream so the count reflects real exposure. A "" authID is a
// no-op.
func (g *AccountConcurrencyGate) RecordRequest(authID string) {
	if authID == "" {
		return
	}
	day := g.currentDay()
	g.mu.Lock()
	entry := g.daily[authID]
	if entry == nil || entry.day != day {
		entry = &dailyCounter{day: day}
		g.daily[authID] = entry
	}
	entry.count++
	g.mu.Unlock()
}

// DailyCount returns how many requests authID has recorded so far during the
// current UTC day (0 if none today, including when its only recorded requests
// were on an earlier day). A "" authID returns 0.
func (g *AccountConcurrencyGate) DailyCount(authID string) int {
	if authID == "" {
		return 0
	}
	day := g.currentDay()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.dailyCountLocked(authID, day)
}

// dailyCountLocked reads authID's count for the given UTC day, treating a
// stale-day entry as 0. Caller must hold g.mu.
func (g *AccountConcurrencyGate) dailyCountLocked(authID string, day int64) int {
	entry := g.daily[authID]
	if entry == nil || entry.day != day {
		return 0
	}
	return entry.count
}

// OverDailyBudget reports whether authID has met or exceeded a positive daily
// budget for the current UTC day. A non-positive budget means "unbounded"
// (mature accounts, design §5.1: quota headroom governs, not a fixed daily
// cap) and always returns false. A "" authID returns false.
func (g *AccountConcurrencyGate) OverDailyBudget(authID string, budget int) bool {
	if authID == "" || budget <= 0 {
		return false
	}
	day := g.currentDay()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.dailyCountLocked(authID, day) >= budget
}

// ---------------------------------------------------------------------------
// Manager wiring: the execution/completion path drives the gate the active
// AdaptiveSelector owns, so the selector's Pick-time avoidance and the
// execution path's acquire/release/record all share one live count.
// ---------------------------------------------------------------------------

// accountGateProvider is implemented by a selector that owns an
// AccountConcurrencyGate (the AdaptiveSelector). The Manager type-asserts its
// current selector to this so the execution path can reach the same gate the
// selector gates against, without a back-reference from the selector to the
// Manager. A non-adaptive selector (round-robin/fill-first) does not implement
// it, so the gate is transparently absent and no gating happens (design D7).
type accountGateProvider interface {
	AccountGate() *AccountConcurrencyGate
}

// accountConcurrencyGate returns the gate owned by the active selector, or nil
// when the current selector is not the adaptive one (in which case the whole
// concurrency/daily-budget mechanism is inert -- the pre-adaptive behavior).
func (m *Manager) accountConcurrencyGate() *AccountConcurrencyGate {
	if m == nil {
		return nil
	}
	if provider, ok := m.Selector().(accountGateProvider); ok && provider != nil {
		return provider.AccountGate()
	}
	return nil
}

// accountSchedulingConfig reads the live AccountSchedulingConfig from the
// runtime config snapshot (the same snapshot the rest of the execution path
// reads). An unset/zero config yields a zero AccountSchedulingConfig, which
// resolves every account to the mature ceiling with a 0 concurrency limit and
// 0 daily budget -- i.e. no gating -- a safe default.
func (m *Manager) accountSchedulingConfig() internalconfig.AccountSchedulingConfig {
	if cfg, ok := m.runtimeConfig.Load().(*internalconfig.Config); ok && cfg != nil {
		return cfg.AccountScheduling
	}
	return internalconfig.AccountSchedulingConfig{}
}

// accountExecutionSlot is a one-request handle over the gate: it remembers which
// account's slot was taken so recordRequest and release act on the same authID
// and the same gate instance, even if the Manager's selector is swapped
// mid-request (the captured gate pointer, not a fresh lookup, is released). A
// nil slot (no active gate) makes every method a no-op, so callers need no
// gate-presence branching.
type accountExecutionSlot struct {
	gate     *AccountConcurrencyGate
	authID   string
	released bool
}

// beginAccountExecution reserves one in-flight concurrency slot for auth on the
// active gate and reports whether the account is still within its concurrency
// ceiling after the reservation. It returns a nil slot (and within=true) when
// no gate is active or auth has no ID, so the non-adaptive path is unaffected.
//
// The concurrency limit is the account's current warm-up stage (or mature)
// ConcurrencyLimit -- AccountWarmupStatus already resolves that single value
// for both warming and mature accounts.
//
// The caller MUST call slot.release() exactly once when the request's in-flight
// lifetime ends (via defer on the non-stream path; plumbed into the stream
// wrapper's completion on the stream path). It should call slot.recordRequest()
// once at the point a request is actually sent upstream. When within is false
// the caller may release immediately and fail over (non-stream, nothing sent
// yet) instead of proceeding over the ceiling.
func (m *Manager) beginAccountExecution(auth *Auth) (*accountExecutionSlot, bool) {
	gate := m.accountConcurrencyGate()
	if gate == nil || auth == nil || auth.ID == "" {
		return nil, true
	}
	cfg := m.accountSchedulingConfig()
	// Only gate the providers this scheduler actually manages (positive
	// configured tier weight -- claude/codex today), matching the selector's
	// adaptiveEligible. A non-adaptive provider (gemini/antigravity/...) is left
	// entirely ungated so its concurrency/daily counting and any failover are
	// never surprise-applied to it.
	if auth.AccountTierBaseWeight(cfg.TierWeights) <= 0 {
		return nil, true
	}
	limit := AccountWarmupStatusFor(auth, time.Now(), cfg).ConcurrencyLimit
	// Scale the concurrency ceiling by the per-account rate multiplier (§8.3) so
	// the execution-path acquire enforces the SAME scaled limit the selector's
	// Pick-time hasConcurrencyHeadroom read avoids against (one source of truth).
	limit = scaleLimitInt(limit, AccountRateScale(auth, cfg))
	within := gate.Acquire(auth.ID, limit)
	return &accountExecutionSlot{gate: gate, authID: auth.ID}, within
}

// recordRequest counts this account's request against its UTC-daily budget. Safe
// on a nil slot.
func (s *accountExecutionSlot) recordRequest() {
	if s == nil || s.gate == nil {
		return
	}
	s.gate.RecordRequest(s.authID)
}

// release drops the in-flight slot exactly once. Safe on a nil slot and safe to
// call more than once (only the first call decrements). It is the release point
// deferred/plumbed onto every execution exit path so a slot is never leaked --
// a leaked slot would leave the account permanently counted as busy and drop it
// out of selection forever.
func (s *accountExecutionSlot) release() {
	if s == nil || s.gate == nil || s.released {
		return
	}
	s.released = true
	s.gate.Release(s.authID)
}

// errAccountConcurrencyBusy is the retryable error the non-stream execution path
// records when an account is at its concurrency ceiling and the request fails
// over to another credential. It is retryable so a genuine full-fleet moment
// surfaces as backpressure the caller can retry, never a hard/terminal failure.
func errAccountConcurrencyBusy(authID string) error {
	message := "account concurrency limit reached, failing over"
	if authID != "" {
		message = "account " + authID + " concurrency limit reached, failing over"
	}
	return &Error{
		Code:       "account_concurrency_exceeded",
		Message:    message,
		Retryable:  true,
		HTTPStatus: http.StatusTooManyRequests,
	}
}

// errAccountDailyBudgetExhausted is the retryable error the adaptive selector
// returns when the ONLY accounts able to serve a request are still-warming
// accounts that have all spent their UTC-daily warm-up budget. In that thin-pool
// case the empty-candidate branch would otherwise degrade to the round-robin
// fallback, which re-picks over the full pool and ignores the daily budget --
// hammering the very account warm-up is protecting (only the concurrency=1 gate
// left as a backstop). Denying instead keeps the account protected.
//
// It deliberately reuses errAccountConcurrencyBusy's failover semantics (Retryable
// + 429, no RetryAfter) rather than inventing a new mechanism, so a genuinely
// budget-exhausted moment surfaces as backpressure the caller can retry, never a
// hard/terminal failure. A distinct Code is kept only so the two protective
// denials are legible apart in logs and client errors. Like the concurrency
// error, it carries no RetryAfter, so Manager.shouldRetryAfterError does not spin
// on it (a 429 with no cooldown target and no RetryAfter is not retried in place)
// and it surfaces to the client as backpressure.
func errAccountDailyBudgetExhausted() error {
	return &Error{
		Code:       "account_daily_budget_exhausted",
		Message:    "all serving accounts are warming and over their daily budget, failing over",
		Retryable:  true,
		HTTPStatus: http.StatusTooManyRequests,
	}
}
