package management

import (
	"context"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// managedHeaderSyncSlowListThresholdMS controls when ListAuthFiles starts
// emitting a warning log line because the supposedly fast path crossed a
// configured budget. The number is intentionally conservative; the historical
// regression that motivated this scheduler observed list latencies above 20s.
const managedHeaderSyncSlowListThresholdMS int64 = 750

// managedHeaderSyncCooldownOnSuccess controls how long the scheduler waits
// before re-running the managed-header sync for the same auth after a
// successful pass. It guards against thrash when ListAuthFiles is polled
// aggressively by dashboards.
const managedHeaderSyncCooldownOnSuccess = 30 * time.Second

// managedHeaderSyncCooldownInitialFailure and managedHeaderSyncCooldownMaxFailure
// bound the exponential back-off applied when sync attempts keep failing
// (e.g. the configured proxy is unreachable so the managed-header projection
// cannot resolve its online version).
const (
	managedHeaderSyncCooldownInitialFailure = 60 * time.Second
	managedHeaderSyncCooldownMaxFailure     = 10 * time.Minute
)

// managedHeaderSyncWorkerTimeout caps each background sync attempt so a
// stalled outbound dependency does not pile up goroutines.
const managedHeaderSyncWorkerTimeout = 25 * time.Second

type managedHeaderSyncState struct {
	inFlight     bool
	failureCount int
	lastSuccess  time.Time
	lastFailure  time.Time
	nextEligible time.Time
}

type managedHeaderSyncScheduler struct {
	mu     sync.Mutex
	states map[string]*managedHeaderSyncState
}

func newManagedHeaderSyncScheduler() *managedHeaderSyncScheduler {
	return &managedHeaderSyncScheduler{
		states: make(map[string]*managedHeaderSyncState),
	}
}

// shouldEnqueue checks if a sync attempt for the given auth ID is allowed
// right now. It atomically marks the auth as in-flight when true so that
// concurrent ListAuthFiles requests cannot double-dispatch the same job.
func (s *managedHeaderSyncScheduler) shouldEnqueue(authID string, now time.Time) bool {
	if s == nil || authID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[authID]
	if !ok {
		state = &managedHeaderSyncState{}
		s.states[authID] = state
	}
	if state.inFlight {
		return false
	}
	if !state.nextEligible.IsZero() && now.Before(state.nextEligible) {
		return false
	}
	state.inFlight = true
	return true
}

func (s *managedHeaderSyncScheduler) recordSuccess(authID string, now time.Time) {
	if s == nil || authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[authID]
	if !ok {
		state = &managedHeaderSyncState{}
		s.states[authID] = state
	}
	state.inFlight = false
	state.failureCount = 0
	state.lastSuccess = now
	state.nextEligible = now.Add(managedHeaderSyncCooldownOnSuccess)
}

func (s *managedHeaderSyncScheduler) recordFailure(authID string, now time.Time) {
	if s == nil || authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[authID]
	if !ok {
		state = &managedHeaderSyncState{}
		s.states[authID] = state
	}
	state.inFlight = false
	state.failureCount++
	state.lastFailure = now
	cooldown := managedHeaderSyncCooldownInitialFailure
	for i := 1; i < state.failureCount && cooldown < managedHeaderSyncCooldownMaxFailure; i++ {
		cooldown *= 2
	}
	if cooldown > managedHeaderSyncCooldownMaxFailure {
		cooldown = managedHeaderSyncCooldownMaxFailure
	}
	state.nextEligible = now.Add(cooldown)
}

func (s *managedHeaderSyncScheduler) clear(authID string) {
	if s == nil || authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.states, authID)
}

// managedHeaderSyncSchedulerForHandler lazily attaches a scheduler to the
// handler. Returning nil when the handler is nil keeps fast-path callers free
// of nil checks.
func (h *Handler) managedHeaderSyncSchedulerForHandler() *managedHeaderSyncScheduler {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.managedHeaderScheduler == nil {
		h.managedHeaderScheduler = newManagedHeaderSyncScheduler()
	}
	return h.managedHeaderScheduler
}

// scheduleManagedHeaderSync queues an asynchronous managed-header sync for
// the given auth when no recent sync attempt is pending or cooling down. It
// is safe to call repeatedly: the scheduler enforces per-auth in-flight
// dedup and exponential failure back-off so misbehaving accounts cannot
// starve the worker pool.
func (h *Handler) scheduleManagedHeaderSync(auth *coreauth.Auth) {
	if h == nil || auth == nil {
		return
	}
	scheduler := h.managedHeaderSyncSchedulerForHandler()
	if scheduler == nil {
		return
	}
	authID := auth.ID
	if authID == "" {
		return
	}
	if !scheduler.shouldEnqueue(authID, time.Now()) {
		return
	}
	// Detach from the request context so the goroutine outlives the caller
	// and so the scheduler cannot be poisoned by a request-level cancellation.
	clone := auth.Clone()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), managedHeaderSyncWorkerTimeout)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				log.WithFields(log.Fields{
					"auth_id":  authID,
					"provider": clone.Provider,
					"panic":    r,
				}).Error("managed header sync worker panicked")
				scheduler.recordFailure(authID, time.Now())
			}
		}()
		updated := h.syncAuthManagedHeaderState(ctx, clone)
		now := time.Now()
		// syncAuthManagedHeaderState returns the same pointer on no-op; we
		// treat that as success since the data is already coherent.
		if updated == nil {
			scheduler.recordFailure(authID, now)
			return
		}
		scheduler.recordSuccess(authID, now)
	}()
}
