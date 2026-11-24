package auth

import (
	"container/list"
	"context"
	"net/http"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const ordinaryCredentialQueueSize = 64
const ordinaryCredentialQueueTimeout = 30 * time.Second

type ordinaryCredentialAdmission struct {
	mu    sync.Mutex
	lanes map[string]*ordinaryCredentialLane
}

type ordinaryCredentialLane struct {
	running int
	waiters list.List
	changed chan struct{}
}

func (a *ordinaryCredentialAdmission) acquire(ctx context.Context, id string, limit func() int) (func(), bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	a.mu.Lock()
	if a.lanes == nil {
		a.lanes = make(map[string]*ordinaryCredentialLane)
	}
	lane := a.lanes[id]
	if lane == nil {
		lane = &ordinaryCredentialLane{changed: make(chan struct{})}
		a.lanes[id] = lane
	}
	capacity := limit()
	if lane.waiters.Len() == 0 && (capacity <= 0 || lane.running < capacity) {
		lane.running++
		a.mu.Unlock()
		return a.release(id, lane), false, nil
	}
	if lane.waiters.Len() >= ordinaryCredentialQueueSize {
		a.mu.Unlock()
		return nil, false, ordinaryQueueError("credential_queue_full", "selected credential queue is full")
	}
	waiter := lane.waiters.PushBack(struct{}{})
	wait, cancel := context.WithTimeout(ctx, ordinaryCredentialQueueTimeout)
	defer cancel()
	for {
		if wait.Err() != nil {
			lane.waiters.Remove(waiter)
			a.notify(id, lane)
			a.mu.Unlock()
			if err := ctx.Err(); err != nil {
				return nil, true, err
			}
			return nil, true, ordinaryQueueError("credential_queue_timeout", "selected credential queue wait expired")
		}
		capacity = limit()
		if lane.waiters.Front() == waiter && (capacity <= 0 || lane.running < capacity) {
			lane.waiters.Remove(waiter)
			lane.running++
			a.notify(id, lane)
			a.mu.Unlock()
			return a.release(id, lane), true, nil
		}
		changed := lane.changed
		a.mu.Unlock()
		select {
		case <-changed:
		case <-wait.Done():
		}
		a.mu.Lock()
	}
}

func (a *ordinaryCredentialAdmission) wakeWaiters() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, lane := range a.lanes {
		if lane.waiters.Len() != 0 {
			a.notify(id, lane)
		}
	}
}

func (a *ordinaryCredentialAdmission) release(id string, lane *ordinaryCredentialLane) func() {
	return sync.OnceFunc(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		lane.running--
		a.notify(id, lane)
	})
}

func (a *ordinaryCredentialAdmission) notify(id string, lane *ordinaryCredentialLane) {
	close(lane.changed)
	if lane.running == 0 && lane.waiters.Len() == 0 {
		delete(a.lanes, id)
		return
	}
	lane.changed = make(chan struct{})
}

func ordinaryQueueError(code, message string) error {
	return wrapRequestStopError(&Error{Code: code, Message: message, HTTPStatus: http.StatusServiceUnavailable})
}

func (m *Manager) admitOrdinaryCredential(ctx context.Context, selected *Auth, model string, homeMode bool) (func(), error) {
	limit := func() int {
		if cfg := m.runtimeConfigSnapshot(); cfg != nil {
			return cfg.Routing.MaxConcurrentPerCredential
		}
		return 0
	}
	if homeMode || selected == nil || selected.ID == "" || limit() <= 0 {
		return nil, nil
	}
	release, waited, err := m.ordinaryCredentialAdmission.acquire(ctx, selected.ID, limit)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	if !waited {
		return release, nil
	}
	current, exists := m.GetByID(selected.ID)
	if !exists || current == nil || current.Disabled || current.Status == StatusDisabled ||
		current.RegistrationEpoch != selected.RegistrationEpoch || current.CredentialVersion != selected.CredentialVersion {
		release()
		return nil, ordinaryQueueError("queued_credential_changed", "selected credential changed while waiting")
	}
	model = m.selectionModelForAuth(current, model)
	now := time.Now()
	if blocked, _, until := isAuthBlockedForModelForRequest(ctx, current, model, now); blocked {
		release()
		if prior := latestCandidateErrorForModel([]*Auth{current}, func(*Auth) string { return model }); isOrdinaryUpstreamRefusal(prior) {
			if wait := until.Sub(now); wait > 0 {
				return nil, wrapRequestStopError(withTrustedRetryAfter(prior, &wait))
			}
			return nil, wrapRequestStopError(prior)
		}
		return nil, ordinaryQueueError("queued_credential_unavailable", "selected credential became unavailable while waiting")
	}
	return release, nil
}

func holdOrdinaryCredentialStream(ctx context.Context, result *cliproxyexecutor.StreamResult, release func()) *cliproxyexecutor.StreamResult {
	if release == nil {
		return result
	}
	if result == nil || result.Chunks == nil {
		release()
		return result
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer release()
		forward := true
		for forward {
			select {
			case <-ctx.Done():
				forward = false
			case chunk, ok := <-result.Chunks:
				if !ok {
					return
				}
				select {
				case <-ctx.Done():
					forward = false
				case out <- chunk:
				}
			}
		}
		// Match the Manager's stream boundary: let its inner wrapper cancel
		// the attempt and finish response bookkeeping before releasing.
		drainStreamChunks(result.Chunks)
	}()
	return &cliproxyexecutor.StreamResult{Chunks: out, Headers: result.Headers}
}
