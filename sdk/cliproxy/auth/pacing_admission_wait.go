package auth

import (
	"context"
	"errors"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const pacingPendingBytesLimit = 32 << 20

type pacingAdmissionState struct {
	started   time.Time
	deadline  time.Time
	config    internalconfig.WarmupAdmissionWaitConfig
	bodyBytes int
	pending   *warmupSelectionWait
	queue     *pacingAdmissionQueue
}

type pacingAdmissionTicket struct {
	request *warmupRequestState
	wait    warmupSelectionWait
	bytes   int
}

type pacingAdmissionQueue struct {
	mu      sync.Mutex
	tickets []*pacingAdmissionTicket
	bytes   int
	changed chan struct{}
}

var sharedPacingAdmissionQueue pacingAdmissionQueue

func pacingAdmissionExpired(ctx context.Context) bool {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.admission != nil && !state.admission.started.IsZero() && state.admission.config.MaxWaitSeconds > 0 && !state.now().Before(state.admission.deadline)
}

func pacingRetryFits(ctx context.Context, delay time.Duration) bool {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return true
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.admission == nil || state.admission.started.IsZero() || (delay > 0 && delay < state.admission.deadline.Sub(state.now()))
}

func configurePacingAdmission(ctx context.Context, cfg internalconfig.WarmupTrafficPacingConfig, bodyBytes int) {
	state := warmupRequestFromContext(ctx)
	if state == nil || !cfg.Enabled {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	w := cfg.WaitConfig()
	if state.admission == nil {
		state.admission = &pacingAdmissionState{config: w, bodyBytes: bodyBytes, queue: &sharedPacingAdmissionQueue}
	} else {
		// Reload may shorten the remaining window but never create another one.
		state.admission.config = w
		state.admission.bodyBytes = max(state.admission.bodyBytes, bodyBytes)
		if !state.admission.started.IsZero() {
			shorter := state.admission.started.Add(time.Duration(w.MaxWaitSeconds) * time.Second)
			if shorter.Before(state.admission.deadline) {
				state.admission.deadline = shorter
			}
		}
	}
}

func activatePacingAdmission(ctx context.Context) {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if a := state.admission; a != nil && a.started.IsZero() {
		a.started = state.now()
		a.deadline = a.started.Add(time.Duration(a.config.MaxWaitSeconds) * time.Second)
	}
}

func (q *pacingAdmissionQueue) signalLocked() {
	if q.changed != nil {
		close(q.changed)
	}
	q.changed = make(chan struct{})
}

func notifyPacingAdmission() {
	q := &sharedPacingAdmissionQueue
	q.mu.Lock()
	q.signalLocked()
	q.mu.Unlock()
}

func (q *pacingAdmissionQueue) release(state *warmupRequestState) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, t := range q.tickets {
		if t.request == state {
			q.bytes -= t.bytes
			q.tickets = append(q.tickets[:i], q.tickets[i+1:]...)
			q.signalLocked()
			return
		}
	}
}

func releasePacingAdmission(ctx context.Context) {
	if state := warmupRequestFromContext(ctx); state != nil {
		state.mu.Lock()
		a := state.admission
		state.mu.Unlock()
		if a != nil {
			a.queue.release(state)
		}
	}
}

func (q *pacingAdmissionQueue) acquire(state *warmupRequestState, wait *warmupSelectionWait, a pacingAdmissionState) (chan struct{}, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	count := 0
	var own *pacingAdmissionTicket
	for _, t := range q.tickets {
		if t.request == state {
			own = t
			continue
		}
		if t.wait.selector == wait.selector && t.wait.authID == wait.authID {
			count++
		}
	}
	if count >= a.config.MaxPendingPerAccount || (own == nil && len(q.tickets) >= a.config.MaxPendingTotal) || (own == nil && a.bodyBytes > pacingPendingBytesLimit-q.bytes) {
		return nil, false
	}
	if own == nil {
		own = &pacingAdmissionTicket{request: state, bytes: a.bodyBytes}
		q.tickets = append(q.tickets, own)
		q.bytes += a.bodyBytes
	}
	own.wait = *wait
	if q.changed == nil {
		q.changed = make(chan struct{})
	}
	return q.changed, true
}

func (q *pacingAdmissionQueue) waiting(s *AdaptiveSelector, authID string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, t := range q.tickets {
		if t.wait.selector == s && t.wait.authID == authID {
			n++
		}
	}
	return n
}

// Probe callbacks run without the queue mutex; they only observe gates. Every
// woken request still rechecks the real atomic execution/attempt reservation.
func pacingQueueTurn(ctx context.Context, s *AdaptiveSelector, authID, key string) bool {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return true
	}
	state.mu.Lock()
	a := state.admission
	state.mu.Unlock()
	if a == nil {
		return true
	}
	q := a.queue
	q.mu.Lock()
	var before []warmupSelectionWait
	for _, t := range q.tickets {
		if t.request == state {
			break
		}
		if t.wait.selector == s && t.wait.authID == authID {
			before = append(before, t.wait)
		}
	}
	q.mu.Unlock()
	for _, w := range before {
		if key != "" && key == w.key {
			return false
		}
		if w.ready != nil && w.ready() {
			return false
		}
	}
	return true
}

func localAdmissionError(reason string, contexts ...context.Context) error {
	retry := time.Duration(0)
	code := "warmup_busy"
	if reason == "concurrency" {
		code = "account_concurrency_exceeded"
	}
	if reason == "queue_full" {
		code = "queue_full"
	}
	if len(contexts) > 0 {
		ctx := contexts[0]
		account := ""
		waited := time.Duration(0)
		depth := 0
		if state := warmupRequestFromContext(ctx); state != nil {
			state.mu.Lock()
			proof, a := state.proof, state.admission
			waited = state.waited
			state.mu.Unlock()
			if proof != nil {
				if reason == proof.reason && (reason == "rpm" || reason == "request-balance" || reason == "daily-budget") {
					retry = proof.delay
				}
				account = pacingDigest(proof.authID)[:12]
				if a != nil {
					depth = a.queue.waiting(proof.selector, proof.authID)
				}
			}
		}
		selectorLogEntry(ctx).Infof("local-admission-denied | source=local_admission account=%s reason=%s waited-ms=%d queue=%d", account, reason, waited.Milliseconds(), depth)
	}
	return &warmupBusyError{cause: &Error{Code: code, Message: "local admission: " + reason, Retryable: true, HTTPStatus: 429}, local: true, delay: retry}
}

func pacingWaitEligible(reason string, delay, remaining time.Duration) bool {
	switch reason {
	case "", "concurrency", "binding_busy", "active-groups", "queue_turn":
		return remaining > 0
	case "rpm", "request-balance", "daily-budget":
		return delay > 0 && delay <= remaining
	}
	return false
}

// HTTP hooks return to the conductor immediately. Only after slot.close has
// released the execution reservation may the next selection sleep.
func deferPacingAdmissionWait(ctx context.Context, wait *warmupSelectionWait) error {
	state := warmupRequestFromContext(ctx)
	if state == nil {
		return localAdmissionError("unavailable", ctx)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.admission == nil {
		return newWarmupBusyError()
	}
	state.admission.pending = wait
	state.suppressReserve = true
	return &warmupAdmissionReselect{}
}

func runPacingAdmissionSelection(ctx context.Context, pick func(context.Context) error) error {
	state := warmupRequestFromContext(ctx)
	for {
		if err := ctx.Err(); err != nil {
			releasePacingAdmission(ctx)
			return err
		}
		state.mu.Lock()
		a := *state.admission
		pending := a.pending
		state.admission.pending = nil
		state.mu.Unlock()
		var err error
		if !a.started.IsZero() && a.config.MaxWaitSeconds > 0 && !state.now().Before(a.deadline) {
			releasePacingAdmission(ctx)
			return localAdmissionError("wait_timeout", ctx)
		}
		if pending != nil {
			err = pending
		} else {
			err = pick(ctx)
		}
		var wait *warmupSelectionWait
		if !errors.As(err, &wait) {
			if err != nil {
				releasePacingAdmission(ctx)
			}
			return err
		}
		activatePacingAdmission(ctx)
		state.mu.Lock()
		a = *state.admission
		state.mu.Unlock()
		remaining := a.deadline.Sub(state.now())
		state.mu.Lock()
		state.proof = wait
		state.mu.Unlock()
		if deadline, ok := ctx.Deadline(); ok {
			remaining = min(remaining, time.Until(deadline))
		}
		if !pacingWaitEligible(wait.reason, wait.delay, remaining) {
			releasePacingAdmission(ctx)
			if remaining <= 0 {
				return localAdmissionError("wait_timeout", ctx)
			}
			return localAdmissionError(wait.reason, ctx)
		}
		changed, ok := a.queue.acquire(state, wait, a)
		if !ok {
			releasePacingAdmission(ctx)
			return localAdmissionError("queue_full", ctx)
		}
		state.mu.Lock()
		state.proof = wait
		state.suppressReserve = true
		state.mu.Unlock()
		delay := wait.delay
		if wait.reason == "" || wait.reason == "concurrency" || wait.reason == "binding_busy" || wait.reason == "active-groups" || wait.reason == "queue_turn" {
			delay = time.Second
		}
		delay = min(delay, remaining)
		start := state.now()
		selectorLogEntry(ctx).Infof("local-admission-wait | source=local_admission account=%s reason=%s remaining-ms=%d queue=%d", pacingDigest(wait.authID)[:12], wait.reason, remaining.Milliseconds(), a.queue.waiting(wait.selector, wait.authID))
		// Tests inject a deterministic sleep; real waits also react to releases.
		// The selection error is a consumed wait signal, not a sleep failure.
		var sleepErr error
		if state.admissionSleep != nil {
			sleepErr = state.admissionSleep(ctx, changed, delay)
		} else {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				sleepErr = ctx.Err()
			case <-changed:
			case <-timer.C:
			}
			timer.Stop()
		}
		state.mu.Lock()
		state.waited += max(0, state.now().Sub(start))
		state.mu.Unlock()
		if sleepErr != nil {
			releasePacingAdmission(ctx)
			return sleepErr
		}
	}
}
