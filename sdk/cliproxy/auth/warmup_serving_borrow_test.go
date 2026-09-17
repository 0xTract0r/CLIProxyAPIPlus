package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func warmupBorrowMature(t *testing.T, f warmupExecutionFixture, suffix string) *Auth {
	t.Helper()
	a := newAdaptiveClaudeAuth(f.a.ID+suffix, "default_claude_max_20x", time.Now().Add(-365*24*time.Hour))
	if _, err := f.m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: f.model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
	return a
}

func TestWarmupServingConcurrentStreamBorrowManager(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "retry"} {
		t.Run(outcome, func(t *testing.T) {
			f := newWarmupExecutionFixture(t, true)
			warmupBorrowMature(t, f, "-mature")
			if outcome == "retry" {
				warmupBorrowMature(t, f, "-mature-second")
			}
			req, opts := f.request()
			upstream := make(chan cliproxyexecutor.StreamChunk, 1)
			upstream <- cliproxyexecutor.StreamChunk{Payload: []byte("data: ready\n\n")}
			f.e.stream = func(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
				if a.ID != f.a.ID || f.s.gate.InFlight(a.ID) != 1 {
					t.Error("original stream lacks warming slot")
				}
				return &cliproxyexecutor.StreamResult{Chunks: upstream}, nil
			}
			result, err := f.m.ExecuteStream(f.ctx, []string{"claude"}, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			<-result.Chunks
			defer func() {
				close(upstream)
				for range result.Chunks {
				}
			}()
			key := "mixed::claude:execution-root::" + f.model
			before, ok := f.s.servingEntry(key, *f.now)
			if !ok || !before.serving.protected || f.s.gate.InFlight(f.a.ID) != 1 {
				t.Fatal("fixture lacks live protected stream")
			}
			calls := 0
			f.e.execute = func(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
				calls++
				if a.ID == f.a.ID {
					t.Error("overlap sent to busy warming account")
				}
				assertWarmupBorrowBinding(t, f, key, before)
				if outcome != "success" && calls == 1 {
					return cliproxyexecutor.Response{}, &Error{HTTPStatus: 503, Message: "synthetic mature failure"}
				}
				return cliproxyexecutor.Response{}, nil
			}
			ctx, state := virtualWarmupWait(f.now)
			_, err = f.m.Execute(ctx, []string{"claude"}, req, opts)
			wantCalls := 1
			if outcome == "retry" {
				wantCalls = 2
			}
			if calls != wantCalls || (err != nil) != (outcome == "failure") || state.waited != 0 {
				t.Fatalf("borrow outcome=%s calls=%d err=%v waited=%v", outcome, calls, err, state.waited)
			}
			assertWarmupBorrowBinding(t, f, key, before)
		})
	}
}

func TestWarmupServingConcurrentBorrowBoundaries(t *testing.T) {
	for _, name := range []string{"no_mature_inflight", "no_mature_waiter", "different_binding", "daily_budget", "disabled", "removed", "default_off"} {
		t.Run(name, func(t *testing.T) {
			s, cfg, now, auths := servingFixture(t)
			opts := servingOptions("root", "", "parent", "Task")
			if servingPick(t, s, opts, auths) != "b-cold" {
				t.Fatal("fixture did not reserve warming")
			}
			key := "claude::claude:root::"
			before, _ := s.servingEntry(key, *now)
			ctx, state := virtualWarmupWait(now)
			if name == "different_binding" || name == "no_mature_waiter" {
				ownerKey := key
				if name == "different_binding" {
					ownerKey += "-other"
				}
				permit, _ := state.queue.acquireFor(&warmupSelectionWait{selector: s, key: ownerKey, authID: "b-cold", revision: before.serving.revision, epoch: s.servingEpoch})
				defer state.queue.release(permit)
			} else {
				s.gate.Acquire("b-cold", 1)
				defer s.gate.Release("b-cold")
			}
			switch name {
			case "no_mature_inflight", "no_mature_waiter":
				auths = auths[1:]
			case "daily_budget":
				for i := 0; i < 200; i++ {
					s.gate.RecordRequest("b-cold")
				}
			case "disabled":
				auths[1].Disabled = true
			case "removed":
				auths = append(auths[:1], auths[2:]...)
			case "default_off":
				cfg.WarmupServingReserve = 0
			}
			var picked *Auth
			err := runWarmupSelection(ctx, func(ctx context.Context) error {
				var err error
				picked, err = s.Pick(ctx, "claude", "", opts, auths)
				return err
			})
			after, _ := s.servingEntry(key, *now)
			if name == "no_mature_inflight" || name == "no_mature_waiter" {
				var busy *warmupBusyError
				if !errors.As(err, &busy) || picked != nil || state.waited != 0 || after.authID != before.authID || *after.serving != *before.serving || !after.expiresAt.Equal(before.expiresAt) {
					t.Fatalf("no-mature overlap changed pin or overflowed: err=%v pick=%v", err, picked)
				}
			} else if err != nil || picked.ID != "a-mature" || after.authID != picked.ID || (after.serving != nil && after.serving.protected) || state.borrowMature {
				t.Fatalf("ordinary hard exit/handoff changed: err=%v pick=%v after=%+v borrow=%v", err, picked, after, state.borrowMature)
			}
			if name == "different_binding" && len(state.queue.accounts) != 1 {
				t.Fatal("second request released the other binding's wait permit")
			}
		})
	}
}

func TestWarmupServingWaitPermitOwnership(t *testing.T) {
	queue := &warmupWaitQueue{}
	wait := &warmupSelectionWait{selector: &AdaptiveSelector{}, key: "binding", authID: "auth", revision: 1, epoch: 2}
	first, same := queue.acquireFor(wait)
	if first == nil || same {
		t.Fatal("first owner not acquired")
	}
	if second, same := queue.acquireFor(wait); second != nil || !same {
		t.Fatal("same binding collision not recognized")
	}
	other := *wait
	other.revision++
	if second, same := queue.acquireFor(&other); second != nil || same {
		t.Fatal("another revision was treated as the same binding")
	}
	queue.release(first)
	second, _ := queue.acquireFor(&other)
	queue.release(first)
	if queue.accounts[wait.authID] != second {
		t.Fatal("stale release removed the new owner")
	}
	queue.release(second)
}

func TestWarmupServingAliasBorrowAndNewChildBinding(t *testing.T) {
	for _, kind := range []string{"alias", "fork", "unknown-child"} {
		t.Run(kind, func(t *testing.T) {
			s, _, now, auths := servingFixture(t)
			parent := servingOptions("root", "", "parent", "Review execution admission.")
			if kind == "alias" {
				parent = cliproxyexecutor.Options{OriginalRequest: []byte(`{"system":"parent","messages":[{"role":"user","content":"Review execution admission."}]}`)}
			}
			if servingPick(t, s, parent, auths) != "b-cold" {
				t.Fatal("fixture lacks protected parent")
			}
			parentID, _ := extractSessionIDs(parent.Headers, parent.OriginalRequest, parent.Metadata)
			parentKey := "claude::" + parentID + "::"
			before, ok := s.servingEntry(parentKey, *now)
			if !ok || !before.serving.protected {
				t.Fatal("protected parent missing")
			}
			opts := servingOptions("root", "worker", "parent", "<fork-boilerplate>Inherited development task</fork-boilerplate>")
			key := warmupChildKey(parentKey, "worker")
			if kind == "alias" {
				opts = cliproxyexecutor.Options{OriginalRequest: []byte(`{"system":"parent","messages":[{"role":"user","content":"Review execution admission."},{"role":"assistant","content":"Checking cancellation."},{"role":"user","content":"Continue the review."}]}`)}
				primary, fallback := extractSessionIDs(opts.Headers, opts.OriginalRequest, opts.Metadata)
				if primary == parentID || fallback != parentID {
					t.Fatal("fixture does not inherit a fallback alias")
				}
				key = "claude::" + primary + "::"
			} else if kind == "unknown-child" {
				opts = servingShapeOptions(t, opts, map[string]any{"role": "user", "content": []any{map[string]any{"type": "unknown-extension", "value": "synthetic"}}})
			}
			s.gate.Acquire("b-cold", 1)
			ctx, request := virtualWarmupWait(now)
			var picked *Auth
			err := runWarmupSelection(ctx, func(ctx context.Context) error {
				var err error
				picked, err = s.Pick(ctx, "claude", "", opts, auths)
				return err
			})
			s.gate.Release("b-cold")
			if err != nil || picked == nil || picked.ID != "a-mature" {
				t.Fatalf("inherited overlap did not select mature: err=%v", err)
			}
			created, exists := s.servingEntry(key, *now)
			if kind == "alias" {
				if exists || request.waited != 0 || !request.borrowMature {
					t.Fatalf("alias overlap committed or waited: exists=%v waited=%v borrow=%v", exists, request.waited, request.borrowMature)
				}
			} else if !exists || created.authID != "a-mature" || !created.serving.parentAffine || created.serving.protected || request.borrowMature || request.waited != warmupWaitLimit {
				t.Fatal("new child borrowed instead of establishing its own stable binding")
			}
			after, ok := s.servingEntry(parentKey, *now)
			if !ok || after.authID != before.authID || *after.serving != *before.serving || !after.expiresAt.Equal(before.expiresAt) {
				t.Fatal("borrow changed the parent/alias source binding")
			}
			*now = now.Add(20 * time.Second)
			want := "a-mature"
			if kind == "alias" {
				want = "b-cold"
			}
			if got := servingPick(t, s, opts, auths); got != want {
				t.Fatalf("later request lost its binding: got=%s want=%s", got, want)
			}
			child, _ := s.servingEntry(key, *now)
			if child.serving.protected != (kind == "alias") || (kind != "alias" && !child.serving.parentAffine) {
				t.Fatal("later inheritance lost protection or parent affinity")
			}
		})
	}
}

func TestWarmupServingAliasWaiterBorrow(t *testing.T) {
	s, _, now, auths := servingFixture(t)
	parent := cliproxyexecutor.Options{OriginalRequest: []byte(`{"system":"parent","messages":[{"role":"user","content":"Review execution admission."}]}`)}
	alias := cliproxyexecutor.Options{OriginalRequest: []byte(`{"system":"parent","messages":[{"role":"user","content":"Review execution admission."},{"role":"assistant","content":"Checking cancellation."},{"role":"user","content":"Continue the review."}]}`)}
	primary, fallback := extractSessionIDs(alias.Headers, alias.OriginalRequest, alias.Metadata)
	parentKey, aliasKey := "claude::"+fallback+"::", "claude::"+primary+"::"
	if primary == fallback || fallback == "" || servingPick(t, s, parent, auths) != "b-cold" {
		t.Fatal("fixture lacks an alias source binding")
	}
	before, _ := s.servingEntry(parentKey, *now)
	ctx, state := virtualWarmupWait(now)
	state.sleep = func(_ context.Context, delay time.Duration) error {
		borrowCtx, borrowState := virtualWarmupWait(now)
		borrowState.queue = state.queue
		var picked *Auth
		err := runWarmupSelection(borrowCtx, func(ctx context.Context) error {
			var err error
			picked, err = s.Pick(ctx, "claude", "", alias, auths)
			return err
		})
		if err != nil || picked == nil || picked.ID != "a-mature" || !borrowState.borrowMature || borrowState.waited != 0 {
			t.Fatalf("alias did not recognize the source waiter: err=%v borrow=%v waited=%v", err, borrowState.borrowMature, borrowState.waited)
		}
		if _, exists := s.servingEntry(aliasKey, *now); exists {
			t.Fatal("alias borrowing committed a new binding")
		}
		after, ok := s.servingEntry(parentKey, *now)
		if !ok || *after.serving != *before.serving || !after.expiresAt.Equal(before.expiresAt) || len(state.queue.accounts) != 1 {
			t.Fatal("alias borrowing changed the source binding or its waiter")
		}
		*now = now.Add(delay)
		return nil
	}
	var picked *Auth
	err := runWarmupSelection(ctx, func(ctx context.Context) error {
		var err error
		picked, err = s.Pick(ctx, "claude", "", parent, auths)
		return err
	})
	if err != nil || picked == nil || picked.ID != "b-cold" || state.waited != 20*time.Second || len(state.queue.accounts) != 0 {
		t.Fatalf("source waiter lost its warming binding: err=%v waited=%v", err, state.waited)
	}
	*now = now.Add(20 * time.Second)
	if servingPick(t, s, alias, auths) != "b-cold" {
		t.Fatal("later alias failed to inherit source warming account")
	}
	bound, ok := s.servingEntry(aliasKey, *now)
	if !ok || bound.authID != "b-cold" || !bound.serving.protected {
		t.Fatal("later alias did not establish its normal protected binding")
	}
}

func assertWarmupBorrowBinding(t *testing.T, f warmupExecutionFixture, key string, before sessionEntry) {
	t.Helper()
	after, ok := f.s.servingEntry(key, *f.now)
	if !ok || after.authID != before.authID || after.serving == nil || *after.serving != *before.serving || !after.expiresAt.Equal(before.expiresAt) {
		t.Fatalf("overlap changed original binding: before=%+v after=%+v", before, after)
	}
}

func TestWarmupServingConcurrentWaitBorrowManager(t *testing.T) {
	f := newWarmupExecutionFixture(t, true)
	mature := warmupBorrowMature(t, f, "-mature")
	for i := range f.config.AccountScheduling.WarmupCurve {
		f.config.AccountScheduling.WarmupCurve[i].DailyBudget = 20
	}
	f.m.SetConfig(f.config)
	req, opts := f.request()
	parentOpts := servingOptions("execution-root", "", "Parent instructions", "Implement request pacing.")
	parentCtx, parentState := virtualWarmupWait(f.now)
	parentState.matureOnly = true
	parentReq := req
	parentReq.Payload = parentOpts.OriginalRequest
	if _, err := f.m.Execute(parentCtx, []string{"claude"}, parentReq, parentOpts); err != nil {
		t.Fatal(err)
	}
	parentKey := "mixed::claude:execution-root::" + f.model
	parentBefore, ok := f.s.servingEntry(parentKey, *f.now)
	if !ok || parentBefore.authID != mature.ID {
		t.Fatal("fixture lacks mature parent")
	}
	opts.Headers.Set("X-Claude-Code-Agent-Id", "development-worker")
	sent := []string{}
	f.e.execute = func(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		sent = append(sent, a.ID)
		return cliproxyexecutor.Response{}, nil
	}
	if _, err := f.m.Execute(f.ctx, []string{"claude"}, req, opts); err != nil {
		t.Fatal(err)
	}
	*f.now = f.now.Add(20 * time.Second)
	second, _ := virtualWarmupWait(f.now)
	if _, err := f.m.Execute(second, []string{"claude"}, req, opts); err != nil {
		t.Fatal(err)
	}
	*f.now = f.now.Add(2100 * time.Millisecond)
	mainCtx, mainState := virtualWarmupWait(f.now)
	key := warmupChildKey(parentKey, "development-worker")
	before, ok := f.s.servingEntry(key, *f.now)
	if !ok || !before.serving.protected || before.authID != f.a.ID {
		t.Fatal("fixture lacks protected warming binding")
	}
	waits := 0
	mainState.sleep = func(_ context.Context, delay time.Duration) error {
		waits++
		if waits != 1 || delay < 17900*time.Millisecond || delay > 17900*time.Millisecond+time.Nanosecond {
			t.Fatalf("unexpected main wait: count=%d delay=%v", waits, delay)
		}
		*f.now = f.now.Add(7900 * time.Millisecond)
		borrowCtx, borrowState := virtualWarmupWait(f.now)
		borrowState.queue = mainState.queue
		progress := servingShapeOptions(t, opts, servingShapeText("user", "Review cancellation behavior."), servingShapeText("assistant", "Reviewing execution."), servingShapeText("user", "Describe the current development action briefly."))
		if _, err := f.m.Execute(borrowCtx, []string{"claude"}, req, progress); err != nil {
			t.Fatal(err)
		}
		if sent[len(sent)-1] != mature.ID || borrowState.waited != 0 {
			t.Fatalf("overlap did not immediately borrow mature: sent=%v waited=%v", sent, borrowState.waited)
		}
		assertWarmupBorrowBinding(t, f, key, before)
		assertWarmupBorrowBinding(t, f, parentKey, parentBefore)
		if len(mainState.queue.accounts) != 1 {
			t.Fatal("borrow released the original request's wait permit")
		}
		*f.now = f.now.Add(delay - 7900*time.Millisecond)
		return nil
	}
	if _, err := f.m.Execute(mainCtx, []string{"claude"}, req, opts); err != nil {
		t.Fatal(err)
	}
	if waits != 1 || len(sent) != 4 || sent[0] != f.a.ID || sent[1] != f.a.ID || sent[2] != mature.ID || sent[3] != f.a.ID {
		t.Fatalf("original request lost warming binding: sent=%v waits=%d", sent, waits)
	}
	if len(mainState.queue.accounts) != 0 {
		t.Fatal("wait owner leaked")
	}
	assertWarmupBorrowBinding(t, f, parentKey, parentBefore)
}
