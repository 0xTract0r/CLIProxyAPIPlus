package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// This executor explicitly exercises the SDK contract, not an HTTP transport.
// Native Claude transport coverage lives in the executor's httptest suite.
type pacingAwareExecutor struct {
	warmupFakeExecutor
	sends  atomic.Int32
	before func(context.Context, *Auth)
	finish func(cliproxyexecutor.HTTPAttemptPermit) error
}

func (*pacingAwareExecutor) SupportsHTTPAttemptGate() bool { return true }
func (e *pacingAwareExecutor) attempt(ctx context.Context, a *Auth, r cliproxyexecutor.Request, count bool) (cliproxyexecutor.Response, error) {
	if e.before != nil {
		e.before(ctx, a)
	}
	gate := cliproxyexecutor.HTTPAttemptGateFromContext(ctx)
	if gate == nil {
		e.sends.Add(1)
		return cliproxyexecutor.Response{Payload: []byte("synthetic")}, nil
	}
	permit, err := gate.Before(ctx, cliproxyexecutor.HTTPAttemptInfo{Provider: "claude", Model: r.Model, CountOnly: count, EstimatedTokens: 0, EstimateKnown: true})
	if err != nil {
		return cliproxyexecutor.Response{}, &cliproxyexecutor.HTTPAttemptGateError{Cause: err}
	}
	if err = permit.MarkSent(ctx); err != nil {
		_ = permit.CancelUnsent(ctx)
		return cliproxyexecutor.Response{}, &cliproxyexecutor.HTTPAttemptGateError{Cause: err}
	}
	e.sends.Add(1)
	if e.finish != nil {
		err = e.finish(permit)
	} else {
		err = permit.Finish(ctx, cliproxyexecutor.HTTPAttemptResult{Complete: true, StatusCode: 200})
	}
	// The SDK preserves a successful response if durable settlement fails.
	_ = err
	return cliproxyexecutor.Response{Payload: []byte("synthetic")}, nil
}
func (e *pacingAwareExecutor) Execute(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.attempt(ctx, a, r, false)
}
func (e *pacingAwareExecutor) CountTokens(ctx context.Context, a *Auth, r cliproxyexecutor.Request, o cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.attempt(ctx, a, r, true)
}

func pacingExecutionFixture(t *testing.T) (*Manager, *AdaptiveSelector, *Auth, *pacingAwareExecutor, *time.Time, string) {
	m, s, a, cfg, now := pacingManagerFixture(t)
	cfg.ProxyURL = "http://proxy.invalid:8080"
	m.SetConfig(cfg)
	model := "claude-pacing-" + t.Name()
	registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	e := &pacingAwareExecutor{warmupFakeExecutor: warmupFakeExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "claude"}}}
	m.executors["claude"] = e
	return m, s, a, e, now, model
}

func TestWarmupPacingExecutionZeroAndContinuous(t *testing.T) {
	m, _, a, e, now, model := pacingExecutionFixture(t)
	opts := servingOptions("pacing-root", "", "system", "Review transaction ordering.")
	req := cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}
	if _, err := m.Execute(context.Background(), []string{"claude"}, req, opts); err == nil || e.sends.Load() != 0 {
		t.Fatal("first enable did not start at zero", err)
	}
	*now = now.Add(30 * time.Minute)
	for i := 0; i < 4; i++ {
		ctx, _ := virtualWarmupWait(now)
		if _, err := m.Execute(ctx, []string{"claude"}, req, opts); err != nil {
			t.Fatalf("continuous %d: %v", i, err)
		}
		*now = now.Add(21 * time.Second)
	}
	d, err := m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if err != nil || d.DayRequests != 4 || e.sends.Load() != 4 {
		t.Fatalf("actual attempts or legacy double count: %+v %v sends=%d", d, err, e.sends.Load())
	}
	ctx, _ := virtualWarmupWait(now)
	if _, err = m.Execute(ctx, []string{"claude"}, req, opts); err == nil || e.sends.Load() != 4 {
		t.Fatal("depleted single account overflowed", err)
	}
}

func TestWarmupPacingExecutionCountNoBinding(t *testing.T) {
	m, s, a, e, now, model := pacingExecutionFixture(t)
	*now = now.Add(8 * time.Hour)
	opts := servingOptions("count-root", "", "system", "Count this request.")
	req := cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}
	if _, err := m.ExecuteCount(context.Background(), []string{"claude"}, req, opts); err != nil {
		t.Fatal(err)
	}
	d, _ := m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if e.sends.Load() != 1 || d.DayRequests != 1 || d.ActiveGroups != 0 || len(s.cache.entries) != 0 {
		t.Fatalf("count created group/binding: %+v", d)
	}
}

func TestWarmupPacingExecutionPreparedCredentialChanged(t *testing.T) {
	m, _, a, e, now, model := pacingExecutionFixture(t)
	*now = now.Add(8 * time.Hour)
	var changed atomic.Bool
	e.before = func(ctx context.Context, prepared *Auth) {
		if changed.CompareAndSwap(false, true) {
			updated := prepared.Clone()
			updated.ProxyURL = "http://new-proxy.invalid:8080"
			_, err := m.Update(ctx, updated)
			if err != nil {
				t.Fatal(err)
			}
		} else if prepared.ProxyURL != "http://new-proxy.invalid:8080" {
			t.Fatal("stale prepared proxy sent")
		}
	}
	opts := servingOptions("credentials-root", "", "system", "Review a proxy migration.")
	ctx, _ := virtualWarmupWait(now)
	_, err := m.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}, opts)
	if err != nil || e.sends.Load() != 1 {
		t.Fatalf("credential reselect: sends=%d err=%v", e.sends.Load(), err)
	}
	m.mu.RLock()
	failed := m.auths[a.ID].Failed
	m.mu.RUnlock()
	if failed != 0 {
		t.Fatal("local refusal polluted health")
	}
}

func TestWarmupPacingExecutionLocalPreviousPreserved(t *testing.T) {
	previous := errors.New("synthetic transport failure")
	err := &cliproxyexecutor.HTTPAttemptGateError{Cause: &warmupAdmissionReselect{}, Previous: previous}
	if isWarmupAdmissionReselect(err) || pacingExecutionError(err) != previous {
		t.Fatal("real previous failure swallowed by reselect")
	}
}

func pacingAddMature(t *testing.T, m *Manager, model string, now time.Time) *Auth {
	t.Helper()
	a := newAdaptiveClaudeAuth("zz-mature-"+t.Name(), "default_claude_max_20x", now.Add(-60*24*time.Hour))
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	return a
}

func TestWarmupPacingExecutionStableMatureHandoff(t *testing.T) {
	m, s, a, e, now, model := pacingExecutionFixture(t)
	s.rng = func() float64 { return 0 }
	mature := pacingAddMature(t, m, model, *now)
	*now = now.Add(30 * time.Minute)
	var picked string
	e.before = func(_ context.Context, a *Auth) { picked = a.ID }
	opts := servingOptions("stable-root", "", "system", "Review transaction ordering.")
	req := cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}
	for i := 0; i < 5; i++ {
		ctx, _ := virtualWarmupWait(now)
		if _, err := m.Execute(ctx, []string{"claude"}, req, opts); err != nil {
			t.Fatal(err)
		}
		if i < 4 && picked != a.ID {
			t.Fatalf("premature handoff %d: %s", i, picked)
		}
		*now = now.Add(21 * time.Second)
	}
	if picked != mature.ID {
		t.Fatal("depletion did not hand off", picked)
	}
	*now = now.Add(30 * time.Minute)
	ctx, _ := virtualWarmupWait(now)
	if _, err := m.Execute(ctx, []string{"claude"}, req, opts); err != nil {
		t.Fatal(err)
	}
	if picked != mature.ID {
		t.Fatal("refill stole mature binding")
	}
}

func TestWarmupPacingExecutionForkMembers(t *testing.T) {
	m, s, a, e, now, model := pacingExecutionFixture(t)
	s.rng = func() float64 { return 0 }
	pacingAddMature(t, m, model, *now)
	*now = now.Add(time.Hour)
	parent := servingOptions("family", "", "system", "Review transaction ordering.")
	fork := servingOptions("family", "fork", "system", "Review transaction ordering.")
	fresh := servingOptions("family", "fresh", "different system", "Design an independent parser.")
	var picked string
	e.before = func(_ context.Context, a *Auth) { picked = a.ID }
	for _, opts := range []cliproxyexecutor.Options{parent, fork, fresh} {
		ctx, _ := virtualWarmupWait(now)
		if _, err := m.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}, opts); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(21 * time.Second)
	}
	if picked == a.ID {
		t.Fatal("fresh independent group bypassed active group limit")
	}
	parentKey := "claude::claude:family::" + model
	// Manager uses a Claude-only mixed route, whose namespace remains mixed.
	if _, ok := s.cache.entries[parentKey]; !ok {
		parentKey = "mixed::claude:family::" + model
	}
	if _, ok := s.cache.entries[parentKey]; !ok {
		t.Fatalf("parent fixture key not found: %s", parentKey)
	}
	s.cache.Invalidate(parentKey)
	d, _ := m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if d.DayRequests != 2 || d.ActiveGroups != 1 {
		t.Fatalf("parent exit released fork member: %+v", d)
	}
	s.cache.Invalidate(warmupChildKey(parentKey, "fork"))
	d, _ = m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if d.ActiveGroups != 0 {
		t.Fatal("last member did not release", d)
	}
}

func TestWarmupPacingExecutionFiftyCompetingRequests(t *testing.T) {
	m, _, a, e, now, model := pacingExecutionFixture(t)
	*now = now.Add(time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			opts := servingOptions("burst", "", "system", "Review transaction ordering.")
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, _ = m.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}, opts)
		}()
	}
	wg.Wait()
	d, err := m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if err != nil || e.sends.Load() != 1 || d.DayRequests != 1 || d.InFlight != 0 {
		t.Fatalf("competing attempts bypassed float RPM/concurrency: %+v %v sends=%d", d, err, e.sends.Load())
	}
}

func TestWarmupPacingExecutionOffPermitCannotBypassReenable(t *testing.T) {
	m, _, a, e, now, model := pacingExecutionFixture(t)
	*now = now.Add(time.Hour)
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	var checked bool
	e.before = func(ctx context.Context, _ *Auth) {
		if checked {
			return
		}
		checked = true
		gate := cliproxyexecutor.HTTPAttemptGateFromContext(ctx)
		off := *cfg
		off.AccountScheduling.WarmupTrafficPacing.Enabled = false
		m.SetConfig(&off)
		permit, err := gate.Before(ctx, cliproxyexecutor.HTTPAttemptInfo{Provider: "claude", Model: model, EstimateKnown: true})
		if err != nil {
			t.Fatal(err)
		}
		m.SetConfig(cfg)
		if err = permit.MarkSent(ctx); err == nil {
			t.Fatal("off permit authorized enabled send")
		}
		_ = permit.CancelUnsent(ctx)
	}
	opts := servingOptions("reenable-root", "", "system", "Review enable transitions.")
	ctx, _ := virtualWarmupWait(now)
	_, _ = m.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}, opts)
	d, err := m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if !checked || err != nil || d.DayRequests != int(e.sends.Load()) {
		t.Fatalf("unreserved off/on attempt: %+v %v", d, err)
	}
}

func TestWarmupPacingExecutionSelectorMemberGeneration(t *testing.T) {
	m, old, a, e, now, model := pacingExecutionFixture(t)
	*now = now.Add(time.Hour)
	opts := servingOptions("generation-root", "", "system", "Review binding lifetime.")
	req := cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}
	ctx, _ := virtualWarmupWait(now)
	if _, err := m.Execute(ctx, []string{"claude"}, req, opts); err != nil {
		t.Fatal(err)
	}
	oldLease := warmupBindingFromContext(ctx)
	*now = now.Add(21 * time.Second)
	next := NewAdaptiveSelector(AdaptiveSelectorConfig{Scheduling: m.accountSchedulingConfig(), SessionAffinity: true}, WithAdaptiveClock(func() time.Time { return *now }))
	t.Cleanup(next.Stop)
	m.SetSelector(next)
	ctx, _ = virtualWarmupWait(now)
	if _, err := m.Execute(ctx, []string{"claude"}, req, opts); err != nil {
		t.Fatal(err)
	}
	newLease := warmupBindingFromContext(ctx)
	if old.pacingMemberID(oldLease.key, oldLease.revision) == next.pacingMemberID(newLease.key, newLease.revision) {
		t.Fatal("selector generations reused member ID")
	}
	old.cache.Invalidate(oldLease.key)
	d, _ := m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if e.sends.Load() != 2 || d.ActiveGroups != 1 {
		t.Fatal("old selector cleanup released new member", d)
	}
	next.cache.Invalidate(newLease.key)
	d, _ = m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if d.ActiveGroups != 0 {
		t.Fatal("new member was not independently released", d)
	}
}

func TestWarmupPacingExecutionPersistenceFailure(t *testing.T) {
	for _, stage := range []string{"before", "finish"} {
		t.Run(stage, func(t *testing.T) {
			m, _, a, e, now, model := pacingExecutionFixture(t)
			*now = now.Add(time.Hour)
			store := &pacingMemoryStore{}
			m.pacing.pacer.store = store
			if stage == "before" {
				e.before = func(context.Context, *Auth) { store.fail = true }
			} else {
				e.finish = func(permit cliproxyexecutor.HTTPAttemptPermit) error {
					store.fail = true
					return permit.Finish(context.Background(), cliproxyexecutor.HTTPAttemptResult{Complete: true, StatusCode: 200})
				}
			}
			opts := servingOptions("persistence-root", "", "system", "Review durable accounting.")
			req := cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}
			ctx, _ := virtualWarmupWait(now)
			_, err := m.Execute(ctx, []string{"claude"}, req, opts)
			if stage == "before" && (err == nil || e.sends.Load() != 0) {
				t.Fatal("failed reservation sent", err)
			}
			if stage == "finish" && (err != nil || e.sends.Load() != 1) {
				t.Fatal("settlement failure replaced success", err)
			}
			*now = now.Add(time.Minute)
			ctx, _ = virtualWarmupWait(now)
			if _, err = m.Execute(ctx, []string{"claude"}, req, opts); err == nil {
				t.Fatal("poisoned ledger authorized followup")
			}
			m.mu.RLock()
			failed := m.auths[a.ID].Failed
			m.mu.RUnlock()
			if failed != 0 {
				t.Fatal("local store failure polluted health")
			}
		})
	}
}

func TestWarmupPacingExecutionLegacyInFlightDrains(t *testing.T) {
	m, s, a, e, now, model := pacingExecutionFixture(t)
	*now = now.Add(time.Hour)
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	updated := *cfg
	updated.AccountScheduling.WarmupCurve = append([]internalconfig.AccountWarmupStage(nil), cfg.AccountScheduling.WarmupCurve...)
	for i := range updated.AccountScheduling.WarmupCurve {
		updated.AccountScheduling.WarmupCurve[i].ConcurrencyLimit = 2
	}
	m.SetConfig(&updated)
	// This simulates a pre-enable executor lifetime, which owns no pacing permit.
	s.gate.Acquire(a.ID, 1)
	ctx, state := virtualWarmupWait(now)
	var drained bool
	state.sleep = func(_ context.Context, d time.Duration) error {
		if e.sends.Load() != 0 {
			t.Fatal("paced send preceded legacy drain")
		}
		if !drained {
			s.gate.Release(a.ID)
			drained = true
		}
		*now = now.Add(d)
		return nil
	}
	opts := servingOptions("legacy-root", "", "system", "Review an in-flight upgrade.")
	_, err := m.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}, opts)
	if err != nil || !drained || e.sends.Load() != 1 {
		t.Fatal("legacy overlap bypassed drain", err)
	}
}

func TestWarmupPacingExecutionLegacyBeforeBootstrap(t *testing.T) {
	m, s, a, e, now, model := pacingExecutionFixture(t)
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	off := *cfg
	off.AccountScheduling.WarmupTrafficPacing.Enabled = false
	m.SetConfig(&off)
	*now = now.Add(time.Hour)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	e.stream = func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
		if cliproxyexecutor.HTTPAttemptGateFromContext(ctx) != nil {
			t.Error("default off installed HTTP gate")
		}
		ch := make(chan cliproxyexecutor.StreamChunk, 1)
		close(started)
		go func() {
			<-release
			ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: synthetic\n\n")}
			close(ch)
		}()
		return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
	}
	opts := servingOptions("bootstrap-root", "", "system", "Review streaming startup.")
	req := cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}
	go func() {
		stream, err := m.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
		if err == nil {
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					err = chunk.Err
				}
			}
		}
		done <- err
	}()
	<-started
	if s.gate.InFlight(a.ID) != 0 {
		t.Fatal("fixture already acquired old stream gate")
	}
	*now = now.Add(21 * time.Second)
	m.SetConfig(cfg)
	ctx, state := virtualWarmupWait(now)
	state.purpose = warmupPurposeCount
	var drained bool
	state.sleep = func(_ context.Context, d time.Duration) error {
		if e.sends.Load() != 0 {
			t.Fatal("new send overlapped pre-bootstrap legacy stream")
		}
		if !drained {
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			drained = true
		}
		*now = now.Add(d)
		return nil
	}
	if _, err := m.ExecuteCount(ctx, []string{"claude"}, req, opts); err != nil {
		t.Fatal(err)
	}
	if !drained || e.sends.Load() != 1 {
		t.Fatal("legacy pre-bootstrap call was invisible")
	}
	d, _ := m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if d.DayRequests != 2 {
		t.Fatal("legacy terminal + paced attempt not combined", d)
	}
}

func TestWarmupPacingExecutionAnonymousCannotReuseGroup(t *testing.T) {
	m, _, _, e, now, model := pacingExecutionFixture(t)
	*now = now.Add(time.Hour)
	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"system":"shared","messages":[{"role":"user","content":"Review cancellation."}]}`)}
	primary, _ := extractSessionIDs(nil, opts.OriginalRequest, nil)
	if len(primary) < 4 || primary[:4] != "msg:" {
		t.Fatal("fixture is not anonymous fingerprint")
	}
	for i := 0; i < 2; i++ {
		ctx, _ := virtualWarmupWait(now)
		_, err := m.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}, opts)
		if err == nil {
			t.Fatal("anonymous fingerprint became reusable group")
		}
	}
	if e.sends.Load() != 0 {
		t.Fatal("anonymous calls bypassed admission")
	}
}

// Standard Claude has one prepared model. This direct helper regression covers
// the conductor's generic pool continuation without claiming a production pool.
func TestWarmupPacingExecutionPoolGateReselectKeepsContinuation(t *testing.T) {
	m, s, a, e, now, model := pacingExecutionFixture(t)
	*now = now.Add(time.Hour)
	opts := servingOptions("pool-root", "", "system", "Review model continuation.")
	ctx, _ := virtualWarmupWait(now)
	if _, err := s.Pick(ctx, "claude", model, opts, []*Auth{a}); err != nil {
		t.Fatal(err)
	}
	firstErr := &Error{Code: "synthetic", Message: "synthetic overload", HTTPStatus: 503}
	calls := []string{}
	e.stream = func(ctx context.Context, current *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
		calls = append(calls, req.Model)
		if req.Model == "up-b" {
			updated := current.Clone()
			updated.ProxyURL = "http://changed-proxy.invalid:8080"
			if _, err := m.Update(ctx, updated); err != nil {
				t.Fatal(err)
			}
		}
		gate := cliproxyexecutor.HTTPAttemptGateFromContext(ctx)
		permit, err := gate.Before(ctx, cliproxyexecutor.HTTPAttemptInfo{Provider: "claude", Model: req.Model, EstimateKnown: true})
		if err != nil {
			return nil, &cliproxyexecutor.HTTPAttemptGateError{Cause: err}
		}
		if err = permit.MarkSent(ctx); err != nil {
			_ = permit.CancelUnsent(ctx)
			return nil, &cliproxyexecutor.HTTPAttemptGateError{Cause: err}
		}
		_ = permit.Finish(ctx, cliproxyexecutor.HTTPAttemptResult{Cause: firstErr, StatusCode: 503})
		*now = now.Add(21 * time.Second)
		return nil, firstErr
	}
	_, err := m.executeStreamWithModelPool(ctx, e, a, "claude", cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}, opts, model, "", []string{"up-a", "up-b"}, true, OAuthModelAliasResult{}, true, false)
	var local *warmupAdmissionReselect
	if !errors.As(err, &local) || local.previous != firstErr || len(calls) != 2 || calls[0] != "up-a" || calls[1] != "up-b" {
		t.Fatalf("pool local reselect lost history: calls=%v err=%v", calls, err)
	}
	if !warmupCanResumeCredential(ctx, map[string]struct{}{a.ID: {}}) {
		t.Fatal("sent first model exhausted credential retry allowance")
	}
	models, resumeErr := warmupResumeModels(ctx, a.ID, []string{"up-a", "up-b"})
	if resumeErr != nil || len(models) != 1 || models[0] != "up-b" {
		t.Fatal("reselection replayed failed model", models, resumeErr)
	}
	if len(m.pacingCalls) != 0 {
		t.Fatal("finished pool models leaked executor lifetimes")
	}
}

func TestWarmupPacingExecutionLegacyUnknownBeforeAsyncUsage(t *testing.T) {
	for _, count := range []bool{false, true} {
		t.Run(map[bool]string{false: "generation", true: "count"}[count], func(t *testing.T) {
			m, _, a, _, now, _ := pacingExecutionFixture(t)
			ctx := withWarmupRequestState(context.Background(), warmupPurposeServe)
			if count {
				ctx = withWarmupRequestState(context.Background(), warmupPurposeCount)
			}
			ctx = m.pacingExecutionContext(ctx, a.ID)
			holder, _ := ctx.Value(pacingOwnershipKey{}).(*pacingOwnership)
			m.pacingMu.Lock()
			holder.gated = false
			holder.started = true
			m.pacingMu.Unlock()
			m.finishPacingCall(ctx)
			cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
			updated := *cfg
			updated.AccountScheduling.WarmupCurve = append([]internalconfig.AccountWarmupStage(nil), cfg.AccountScheduling.WarmupCurve...)
			for i := range updated.AccountScheduling.WarmupCurve {
				updated.AccountScheduling.WarmupCurve[i].TokenDailyBudget = 100000
			}
			m.SetConfig(&updated)
			*now = now.Add(time.Hour)
			d, err := m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
			if err != nil {
				t.Fatal(err)
			}
			if !count && d.Reason != "unknown-token-history" {
				t.Fatal("async usage gap spent unknown generation tokens", d)
			}
			if count && !d.Allowed {
				t.Fatal("count created unknown generation debt", d)
			}
		})
	}
}

func TestWarmupPacingExecutionStreamLifetimes(t *testing.T) {
	for _, mode := range []string{"paced", "legacy", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			m, s, a, e, now, model := pacingExecutionFixture(t)
			*now = now.Add(time.Hour)
			if mode == "legacy" {
				cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
				off := *cfg
				off.AccountScheduling.WarmupTrafficPacing.Enabled = false
				m.SetConfig(&off)
			}
			for i := 0; i < 2; i++ {
				release := make(chan struct{})
				e.stream = func(ctx context.Context, _ *Auth, r cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
					var permit cliproxyexecutor.HTTPAttemptPermit
					if gate := cliproxyexecutor.HTTPAttemptGateFromContext(ctx); gate != nil {
						var err error
						permit, err = gate.Before(ctx, cliproxyexecutor.HTTPAttemptInfo{Provider: "claude", Model: r.Model, EstimateKnown: true})
						if err != nil {
							return nil, &cliproxyexecutor.HTTPAttemptGateError{Cause: err}
						}
						if err = permit.MarkSent(ctx); err != nil {
							_ = permit.CancelUnsent(ctx)
							return nil, &cliproxyexecutor.HTTPAttemptGateError{Cause: err}
						}
					}
					e.sends.Add(1)
					ch := make(chan cliproxyexecutor.StreamChunk, 1)
					ch <- cliproxyexecutor.StreamChunk{Payload: []byte("data: synthetic\n\n")}
					go func() {
						complete := true
						select {
						case <-release:
						case <-ctx.Done():
							complete = false
						}
						if permit != nil {
							_ = permit.Finish(ctx, cliproxyexecutor.HTTPAttemptResult{Complete: complete})
						}
						close(ch)
					}()
					return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
				}
				opts := servingOptions("stream-lifetime-root", "", "system", "Review a streaming implementation.")
				ctx, cancel := context.WithCancel(context.Background())
				stream, err := m.ExecuteStream(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}, opts)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				m.pacingMu.Lock()
				active := len(m.pacingCalls)
				m.pacingMu.Unlock()
				if active != 1 || s.gate.InFlight(a.ID) != 1 {
					cancel()
					t.Fatalf("live response lost lifetime: calls=%d gate=%d", active, s.gate.InFlight(a.ID))
				}
				<-stream.Chunks
				if mode == "cancel" {
					cancel()
				} else {
					close(release)
				}
				for range stream.Chunks {
				}
				cancel()
				m.pacingMu.Lock()
				active = len(m.pacingCalls)
				m.pacingMu.Unlock()
				if active != 0 || s.gate.InFlight(a.ID) != 0 {
					t.Fatalf("completed stream retained lifetime: calls=%d gate=%d", active, s.gate.InFlight(a.ID))
				}
				d, err := m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
				if err != nil || d.InFlight != 0 {
					t.Fatal("stream retained pacing permit", d, err)
				}
				*now = now.Add(21 * time.Second)
			}
			if e.sends.Load() != 2 {
				t.Fatal("consecutive streams did not both send")
			}
		})
	}
}

func TestWarmupPacingExecutionReserveToggleKeepsGroup(t *testing.T) {
	m, s, a, _, now, model := pacingExecutionFixture(t)
	*now = now.Add(time.Hour)
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	reserve := *cfg
	reserve.AccountScheduling.WarmupServingReserve = 0.5
	m.SetConfig(&reserve)
	opts := servingOptions("toggle-root", "", "system", "Review independent feature flags.")
	ctx, _ := virtualWarmupWait(now)
	if _, err := m.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}, opts); err != nil {
		t.Fatal(err)
	}
	lease := warmupBindingFromContext(ctx)
	before, _ := s.servingEntry(lease.key, *now)
	epoch := s.servingEpoch
	m.SetConfig(cfg)
	after, ok := s.servingEntry(lease.key, *now)
	if !ok || after.serving == nil || *after.serving != *before.serving || s.servingEpoch != epoch {
		t.Fatal("reserve-off erased active pacing binding")
	}
	d, _ := m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if d.ActiveGroups != 1 {
		t.Fatal("reserve-off released pacing group", d)
	}
	both := *cfg
	both.AccountScheduling.WarmupTrafficPacing.Enabled = false
	m.SetConfig(&both)
	d, _ = m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if d.DayRequests != 1 || d.ActiveGroups != 0 || s.servingEpoch == epoch {
		t.Fatal("full disable lost debt or kept group", d)
	}
}

func TestWarmupPacingExecutionNonAdaptiveStreamDrainsBeforeEnable(t *testing.T) {
	m, adaptive, a, e, now, model := pacingExecutionFixture(t)
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	off := *cfg
	off.AccountScheduling.WarmupTrafficPacing.Enabled = false
	m.SetConfig(&off)
	m.SetSelector(&RoundRobinSelector{})
	*now = now.Add(time.Hour)
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	e.stream = func(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
		if cliproxyexecutor.HTTPAttemptGateFromContext(ctx) != nil {
			t.Error("non-adaptive stream installed an HTTP gate")
		}
		chunks := make(chan cliproxyexecutor.StreamChunk, 1)
		close(started)
		go func() {
			<-release
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("data: synthetic\n\n")}
			close(chunks)
		}()
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}
	opts := servingOptions("strategy-transition", "", "system", "Review a scheduler transition.")
	req := cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}
	go func() {
		stream, err := m.ExecuteStream(context.Background(), []string{"claude"}, req, opts)
		if err == nil {
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					err = chunk.Err
				}
			}
		}
		done <- err
	}()
	<-started
	if m.accountConcurrencyGate() != nil {
		t.Fatal("non-adaptive fixture unexpectedly owns a gate")
	}
	var drained bool
	drain := func() {
		if !drained {
			close(release)
			if err := <-done; err != nil {
				t.Error(err)
			}
			drained = true
		}
	}
	defer drain()
	m.SetSelector(adaptive)
	m.SetConfig(cfg)
	ctx, state := virtualWarmupWait(now)
	state.purpose = warmupPurposeCount
	state.sleep = func(_ context.Context, delay time.Duration) error {
		if e.sends.Load() != 0 {
			t.Error("paced send overlapped a non-adaptive pre-bootstrap stream")
		}
		drain()
		*now = now.Add(delay)
		return nil
	}
	if _, err := m.ExecuteCount(ctx, []string{"claude"}, req, opts); err != nil {
		t.Fatal(err)
	}
	if !drained || e.sends.Load() != 1 {
		t.Fatal("strategy change lost the old native Claude execution")
	}
	if adaptive.gate.InFlight(a.ID) != 0 {
		t.Fatal("strategy transition retained an execution slot")
	}
	m.pacingMu.Lock()
	active := len(m.pacingCalls)
	m.pacingMu.Unlock()
	if active != 0 {
		t.Fatal("strategy transition retained an executor lifetime")
	}
}
