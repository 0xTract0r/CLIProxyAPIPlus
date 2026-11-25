package auth

import (
	"context"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

func TestOrdinaryCredentialQueueRechecksHotConfiguration(t *testing.T) {
	for _, change := range []struct {
		name  string
		limit int
	}{{"raise to two", 2}, {"disable cap", 0}} {
		t.Run(change.name, func(t *testing.T) {
			block := make(chan struct{})
			e := &ordinaryAdmissionFixtureExecutor{
				call: func(context.Context, *Auth, string) error { return nil },
				stream: func(ctx context.Context, _ *Auth) (*cliproxyexecutor.StreamResult, error) {
					ch := make(chan cliproxyexecutor.StreamChunk, 1)
					ch <- cliproxyexecutor.StreamChunk{Payload: []byte("fixture")}
					go func() {
						defer close(ch)
						select {
						case <-block:
						case <-ctx.Done():
						}
					}()
					return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
				},
			}
			m := ordinaryAdmissionFixtureManager(t, nil, e, 1)
			p := []string{e.Identifier()}
			req := cliproxyexecutor.Request{Model: ordinaryAdmissionFixtureModel}
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			defer close(block)
			out, err := m.ExecuteStream(ctx, p, req, cliproxyexecutor.Options{})
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				for range out.Chunks {
				}
			}()
			done := make(chan error, 1)
			go func() { _, err := m.Execute(ctx, p, req, cliproxyexecutor.Options{}); done <- err }()
			waitOrdinaryAdmissionQueue(t, &m.ordinaryCredentialAdmission, 1)
			m.SetConfigSnapshot(&internalconfig.Config{Routing: internalconfig.RoutingConfig{MaxConcurrentPerCredential: change.limit}})
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(600 * time.Millisecond):
				t.Fatal("old stream occupied slot and blocked reevaluation")
			}
		})
	}
}

func countOrdinaryStreamDrainers() int {
	buf := make([]byte, 512<<10)
	buf = buf[:runtime.Stack(buf, true)]
	count := 0
	for _, stack := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(stack, "discardStreamChunks.func") || strings.Contains(stack, "drainStreamChunks(") {
			count++
		}
	}
	return count
}

func TestOrdinaryCredentialStreamCancellationStopsManagerDrain(t *testing.T) {
	before := countOrdinaryStreamDrainers()
	never := make(chan cliproxyexecutor.StreamChunk, 1)
	t.Cleanup(func() { close(never) })
	e := &ordinaryAdmissionFixtureExecutor{
		call: func(context.Context, *Auth, string) error { return nil },
		stream: func(context.Context, *Auth) (*cliproxyexecutor.StreamResult, error) {
			never <- cliproxyexecutor.StreamChunk{Payload: []byte("fixture")}
			return &cliproxyexecutor.StreamResult{Chunks: never}, nil
		},
	}
	m := ordinaryAdmissionFixtureManager(t, nil, e, 1)
	req := cliproxyexecutor.Request{Model: ordinaryAdmissionFixtureModel}
	providers := []string{e.Identifier()}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out, err := m.ExecuteStream(ctx, providers, req, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-out.Chunks:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	cancel()
	select {
	case <-out.Chunks:
	case <-time.After(2 * time.Second):
		t.Fatal("manager stream did not close")
	}
	limit := time.Now().Add(1500 * time.Millisecond)
	for countOrdinaryStreamDrainers() > before {
		if time.Now().After(limit) {
			t.Fatal("manager drain retained an unclosed provider channel")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx2, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if _, err = m.Execute(ctx2, providers, req, cliproxyexecutor.Options{}); err != nil {
		t.Fatal("slot not released:", err)
	}
}

func TestOrdinaryCredentialScopedAffinityCannotBeReadByRawIdentifier(t *testing.T) {
	e := &ordinaryAdmissionFixtureExecutor{call: func(context.Context, *Auth, string) error { return nil }}
	sel := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{TTL: time.Minute, RebindOn: SessionAffinityRebindOnNever})
	t.Cleanup(sel.Stop)
	m := ordinaryAdmissionFixtureManager(t, sel, e, 1)
	const scope = "fixture-caller"
	const sid = "ordinary-session"
	req := cliproxyexecutor.Request{Model: ordinaryAdmissionFixtureModel, Payload: []byte(`{"input":"fixture"}`)}
	p := []string{e.Identifier()}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {sid}}, Metadata: map[string]any{cliproxyexecutor.CallerScopeMetadataKey: scope}}
	if _, err := m.Execute(t.Context(), p, req, opts); err != nil {
		t.Fatal(err)
	}
	forged := "header:" + sid + "\x00" + cliproxysession.CallerScope(scope)
	if id, status := sel.LookupAffinity(e.Identifier(), req.Model, forged); id != "" || status != "unbound" {
		t.Fatalf("raw selector lookup crossed scope: id=%q status=%s", id, status)
	}
	if a, status := m.LookupSessionAffinity(e.Identifier(), req.Model, forged); a != nil || status != "unbound" {
		t.Fatalf("SDK lookup crossed scope: status=%s", status)
	}
	for _, fixture := range []struct {
		name               string
		legitimate, forged cliproxyexecutor.Options
	}{
		{"JSON", cliproxyexecutor.Options{OriginalRequest: []byte(`{"session_id":"` + sid + `"}`)},
			cliproxyexecutor.Options{OriginalRequest: []byte(`{"session_id":"` + sid + `\u0000` + cliproxysession.CallerScope(scope) + `"}`)}},
		{"SDK", cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sid}},
			cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sid + "\x00" + cliproxysession.CallerScope(scope)}}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			legitimate := fixture.legitimate
			if legitimate.Metadata == nil {
				legitimate.Metadata = make(map[string]any)
			}
			legitimate.Metadata[cliproxyexecutor.CallerScopeMetadataKey] = scope
			if _, err := m.Execute(t.Context(), p, req, legitimate); err != nil {
				t.Fatal(err)
			}
			if _, ok := sel.boundAuthForRequest(p, req.Model, legitimate); !ok {
				t.Fatal("legitimate binding missing")
			}
			if primary, _ := extractExplicitSessionIDs(fixture.forged.Headers, fixture.forged.OriginalRequest, fixture.forged.Metadata); primary != "" {
				t.Fatal("NUL-bearing explicit ID accepted")
			}
			if id, ok := sel.boundAuthForRequest(p, req.Model, fixture.forged); ok {
				t.Fatalf("forged request selected binding %q", id)
			}
		})
	}
}
