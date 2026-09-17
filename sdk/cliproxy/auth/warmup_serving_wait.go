package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

const warmupWaitLimit = 30 * time.Second

type warmupRequestPurpose uint8

const (
	warmupPurposeServe warmupRequestPurpose = iota
	warmupPurposeCount
)

type warmupRequestContextKey struct{}

// Shared by all selection/admission attempts of one logical request. Execution
// entry points must install this outside their outer retry loops.
type warmupRequestState struct {
	mu                          sync.Mutex
	purpose                     warmupRequestPurpose
	waited                      time.Duration
	matureOnly, suppressReserve bool
	borrowMature                bool
	borrowProof                 *warmupSelectionWait
	proof                       *warmupSelectionWait
	lease                       warmupBindingLease
	localReselects              int
	sendSerial                  uint64
	modelResume                 *warmupModelResume
	rateAuth                    string
	rateCharged                 bool
	now                         func() time.Time
	sleep                       func(context.Context, time.Duration) error
	queue                       *warmupWaitQueue
}

func withWarmupRequestState(ctx context.Context, purpose warmupRequestPurpose) context.Context {
	if warmupRequestFromContext(ctx) != nil {
		return ctx
	}
	state := &warmupRequestState{purpose: purpose, now: time.Now, sleep: warmupSleep, queue: &sharedWarmupWaitQueue}
	return context.WithValue(ctx, warmupRequestContextKey{}, state)
}

func warmupRequestFromContext(ctx context.Context) *warmupRequestState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(warmupRequestContextKey{}).(*warmupRequestState)
	return state
}

func warmupCountPurpose(ctx context.Context) bool {
	state := warmupRequestFromContext(ctx)
	return state != nil && state.purpose == warmupPurposeCount
}

func warmupSleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type warmupSelectionWait struct {
	selector        *AdaptiveSelector
	key, authID     string
	revision, epoch uint64
	delay           time.Duration
}

func (*warmupSelectionWait) Error() string { return "warming binding temporarily busy" }

type warmupBusyError struct{ cause *Error }

func (e *warmupBusyError) Error() string   { return e.cause.Error() }
func (e *warmupBusyError) StatusCode() int { return e.cause.StatusCode() }
func (e *warmupBusyError) Unwrap() error   { return e.cause }

func newWarmupBusyError() error {
	return &warmupBusyError{&Error{Code: "warmup_busy", Message: "warming binding is busy; retry later", Retryable: true, HTTPStatus: http.StatusTooManyRequests}}
}

func (*warmupBusyError) RetryAfter() *time.Duration { d := time.Second; return &d }

type warmupWaitQueue struct {
	mu       sync.Mutex
	accounts map[string]*warmupWaitPermit
}

type warmupWaitPermit struct{ owner warmupSelectionWait }

func (p *warmupWaitPermit) matches(wait *warmupSelectionWait) bool {
	return p != nil && wait != nil && p.owner.selector == wait.selector && p.owner.key == wait.key && p.owner.authID == wait.authID && p.owner.revision == wait.revision && p.owner.epoch == wait.epoch
}

var sharedWarmupWaitQueue warmupWaitQueue

func (q *warmupWaitQueue) acquire(authID string) bool {
	permit, _ := q.acquireFor(&warmupSelectionWait{authID: authID})
	return permit != nil
}

// The conflict classification and ownership reservation share one lock.
func (q *warmupWaitQueue) acquireFor(wait *warmupSelectionWait) (*warmupWaitPermit, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if owner := q.accounts[wait.authID]; owner != nil {
		return nil, wait.selector != nil && wait.key != "" && wait.revision != 0 && owner.matches(wait)
	}
	if len(q.accounts) >= 64 {
		return nil, false
	}
	if q.accounts == nil {
		q.accounts = make(map[string]*warmupWaitPermit)
	}
	permit := &warmupWaitPermit{owner: *wait}
	q.accounts[wait.authID] = permit
	return permit, false
}

func (q *warmupWaitQueue) release(permit *warmupWaitPermit) {
	if permit == nil {
		return
	}
	q.mu.Lock()
	if q.accounts[permit.owner.authID] == permit {
		delete(q.accounts, permit.owner.authID)
	}
	q.mu.Unlock()
}

// pick must return with every Manager/selector/cache lock released. Every retry
// calls the original picker afresh; no candidate slice survives this wait.
func runWarmupSelection(ctx context.Context, pick func(context.Context) error) error {
	ctx = withWarmupRequestState(ctx, warmupPurposeServe)
	state := warmupRequestFromContext(ctx)
	var held *warmupWaitPermit
	logged, logAuth, outcome := false, "", "complete"
	defer func() {
		if held != nil {
			state.queue.release(held)
		}
		if logged {
			state.mu.Lock()
			waited := state.waited
			state.mu.Unlock()
			selectorLogEntry(ctx).Infof("adaptive-select: warmup-wait-%s | auth=%s waited-ms=%d", outcome, logAuth, waited.Milliseconds())
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			outcome = "cancel"
			return err
		}
		err := pick(ctx)
		var wait *warmupSelectionWait
		if !errors.As(err, &wait) {
			if err != nil && outcome == "complete" {
				if isWarmupAdmissionReselect(err) {
					outcome = "reselect"
				} else {
					outcome = "failed"
				}
			}
			return err
		}
		if !logged {
			logged, logAuth = true, wait.authID
			selectorLogEntry(ctx).Infof("adaptive-select: warmup-wait-start | auth=%s delay-ms=%d", wait.authID, wait.delay.Milliseconds())
		}
		state.mu.Lock()
		state.proof, state.suppressReserve = wait, true
		remaining := warmupWaitLimit - state.waited
		state.mu.Unlock()
		if held != nil && !held.matches(wait) {
			state.queue.release(held)
			held = nil
		}
		sameBinding := false
		if remaining > 0 && held == nil {
			held, sameBinding = state.queue.acquireFor(wait)
		}
		if remaining <= 0 || held == nil {
			outcome = "queue-full"
			if remaining <= 0 {
				outcome = "timeout"
			}
			state.mu.Lock()
			state.matureOnly = true
			if sameBinding {
				state.borrowProof = wait
				outcome = "concurrent-borrow"
			}
			state.mu.Unlock()
			continue
		}
		delay := wait.delay
		if delay <= 0 {
			delay = 100 * time.Millisecond
		}
		if delay > remaining {
			delay = remaining
		}
		started := state.now()
		err = state.sleep(ctx, delay)
		elapsed := state.now().Sub(started)
		if elapsed < 0 {
			elapsed = 0
		}
		state.mu.Lock()
		state.waited += elapsed
		if state.waited >= warmupWaitLimit {
			state.matureOnly = true
			outcome = "timeout"
		}
		state.mu.Unlock()
		if err != nil {
			outcome = "cancel"
			return err
		}
	}
}

func warmupBorrowPolicy(ctx context.Context) (bool, *warmupSelectionWait) {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return false, nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.borrowMature, state.borrowProof
}

// Once borrowed, every retry stays noncommitting even if its mature auth fails.
func warmupMarkBorrow(ctx context.Context) {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return
	}
	state.mu.Lock()
	state.borrowMature, state.suppressReserve = true, true
	state.lease = warmupBindingLease{}
	state.rateAuth, state.rateCharged = "", false
	state.mu.Unlock()
}

func warmupSelectionPolicy(ctx context.Context) (matureOnly, suppress bool, proof *warmupSelectionWait) {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.matureOnly, state.suppressReserve, state.proof
}

func warmupRecordRateCharge(ctx context.Context, authID string, charged bool) {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return
	}
	state.mu.Lock()
	state.rateAuth, state.rateCharged = authID, charged
	state.mu.Unlock()
}

// Admission consumes this one-shot receipt instead of spending a second token.
func warmupRateChargeAvailable(ctx context.Context, authID string) bool {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.rateAuth == authID && state.rateCharged
}

func warmupTakeRateCharge(ctx context.Context, authID string) bool {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	charged := state.rateAuth == authID && state.rateCharged
	state.rateAuth, state.rateCharged = "", false
	return charged
}

// Admission revalidates this immutable selection snapshot; it grants no slot.
type warmupBindingLease struct {
	selector        *AdaptiveSelector
	key, authID     string
	revision, epoch uint64
	protected       bool
}

func warmupRecordBinding(ctx context.Context, selector *AdaptiveSelector, key, authID string, revision, epoch uint64, protected bool) {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return
	}
	state.mu.Lock()
	if state.rateAuth != authID {
		state.rateAuth, state.rateCharged = "", false
	}
	state.lease = warmupBindingLease{selector: selector, key: key, authID: authID, revision: revision, epoch: epoch, protected: protected}
	state.mu.Unlock()
}

func warmupBindingFromContext(ctx context.Context) warmupBindingLease {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return warmupBindingLease{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.lease
}
