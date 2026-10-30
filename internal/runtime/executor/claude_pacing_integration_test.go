package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestClaudePacingManagerNativeStreamWaitResume(t *testing.T) {
	for _, wake := range []string{"release-event", "timer"} {
		t.Run(wake, func(t *testing.T) {
			var sends atomic.Int32
			firstEntered := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := sends.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if wake == "release-event" && attempt == 1 {
					close(firstEntered)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, claudeAttemptStreamUsage)
			}))
			defer server.Close()
			defer releaseFirst()
			cfg := &config.Config{AuthDir: t.TempDir(), AccountScheduling: config.DefaultAccountSchedulingConfig()}
			cfg.AccountScheduling.WarmupTrafficPacing.Enabled = true
			cfg.AccountScheduling.WarmupTrafficPacing.AdmissionWait = &config.WarmupAdmissionWaitConfig{MaxWaitSeconds: 2, MaxPendingPerAccount: 4, MaxPendingTotal: 32}
			for i := range cfg.AccountScheduling.WarmupCurve {
				cfg.AccountScheduling.WarmupCurve[i].DailyBudget = 200
				cfg.AccountScheduling.WarmupCurve[i].RPMLimit = 3
				cfg.AccountScheduling.WarmupCurve[i].ConcurrencyLimit = 1
				cfg.AccountScheduling.WarmupCurve[i].TokenDailyBudget = 100000
			}
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()) }
			selector := cliproxyauth.NewAdaptiveSelector(cliproxyauth.AdaptiveSelectorConfig{Scheduling: cfg.AccountScheduling, SessionAffinity: true}, cliproxyauth.WithAdaptiveClock(now))
			defer selector.Stop()
			manager := cliproxyauth.NewManager(nil, selector, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(NewClaudeExecutor(cfg))
			a := &cliproxyauth.Auth{ID: "native-wait-" + wake, Provider: "claude", Status: cliproxyauth.StatusActive, ProxyURL: "direct", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": server.URL}, Metadata: map[string]any{"quota_snapshot": map[string]any{"profile": map[string]any{"organization": map[string]any{"rate_limit_tier": "default_claude_max_5x"}}}}}
			if _, err := manager.Register(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			model := "claude-3-5-sonnet-20241022"
			registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: model}})
			defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
			clock.Add(int64(30 * time.Minute))
			payload := []byte(strings.ReplaceAll(claudeAttemptRequestJSON, "claude-sonnet-4-6", model))
			req := cliproxyexecutor.Request{Model: model, Payload: payload}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), OriginalRequest: payload, Headers: http.Header{"X-Claude-Code-Session-Id": []string{"native-wait-session"}}}
			invoke := func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				stream, err := manager.ExecuteStream(ctx, []string{"claude"}, req, opts)
				if err != nil {
					return err
				}
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						return chunk.Err
					}
				}
				return nil
			}
			if wake == "release-event" {
				firstDone := make(chan error, 1)
				go func() { firstDone <- invoke() }()
				select {
				case <-firstEntered:
				case <-time.After(time.Second):
					t.Fatal("first native HTTP attempt did not start")
				}
				secondDone := make(chan error, 1)
				go func() { secondDone <- invoke() }()
				queued := false
				deadline := time.Now().Add(time.Second)
				for time.Now().Before(deadline) {
					view := manager.WarmupTrafficPacingSnapshot(a.ID)
					if view.WaitingRequests != nil && *view.WaitingRequests == 1 {
						queued = true
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
				if !queued {
					t.Fatal("second native stream did not enter the production wait loop")
				}
				// Advance only the existing limiter clock, without raising any ceiling.
				clock.Add(int64(21 * time.Second))
				releaseFirst()
				if err := <-firstDone; err != nil {
					t.Fatal("first stream failed", err)
				}
				if err := <-secondDone; err != nil {
					t.Fatal("release event leaked the internal wait instead of resuming", err)
				}
			} else {
				if err := invoke(); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(time.Second)
				for selector.AccountGate().InFlight(a.ID) != 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if selector.AccountGate().InFlight(a.ID) != 0 {
					t.Fatal("first stream retained execution slot")
				}
				clock.Add(int64(19 * time.Second))
				advance := time.AfterFunc(100*time.Millisecond, func() { clock.Add(int64(2 * time.Second)) })
				defer advance.Stop()
				started := time.Now()
				if err := invoke(); err != nil {
					t.Fatal("timer leaked the internal wait instead of resuming", err)
				}
				if time.Since(started) < 900*time.Millisecond {
					t.Fatal("fixture did not exercise the real admission timer")
				}
			}
			if sends.Load() != 2 {
				t.Fatalf("native attempts=%d, want exactly two", sends.Load())
			}
		})
	}
}

func TestClaudePacingManagerNativeHTTP(t *testing.T) {
	for _, entry := range []string{"execute", "stream", "stream-after-admission-deadline", "count", "thinking-clear", "reject-unknown-edit"} {
		t.Run(entry, func(t *testing.T) {
			var sends atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if entry == "stream-after-admission-deadline" {
					// Receiving native HTTP proves MarkSent already completed. The
					// first byte arrives after the one-second admission budget.
					time.Sleep(1200 * time.Millisecond)
				}
				if entry == "count" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"input_tokens":100}`)
					return
				}
				if entry == "stream" || entry == "stream-after-admission-deadline" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, claudeAttemptStreamUsage)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, claudeAttemptResponseJSON)
			}))
			defer server.Close()
			cfg := &config.Config{AccountScheduling: config.DefaultAccountSchedulingConfig()}
			cfg.AuthDir = t.TempDir()
			cfg.AccountScheduling.WarmupTrafficPacing.Enabled = true
			if entry == "stream-after-admission-deadline" {
				cfg.AccountScheduling.WarmupTrafficPacing.AdmissionWait = &config.WarmupAdmissionWaitConfig{MaxWaitSeconds: 1, MaxPendingPerAccount: 4, MaxPendingTotal: 32}
			}
			for i := range cfg.AccountScheduling.WarmupCurve {
				cfg.AccountScheduling.WarmupCurve[i].DailyBudget = 200
				cfg.AccountScheduling.WarmupCurve[i].RPMLimit = 3
				cfg.AccountScheduling.WarmupCurve[i].ConcurrencyLimit = 1
				cfg.AccountScheduling.WarmupCurve[i].TokenDailyBudget = 100000
			}
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()) }
			selector := cliproxyauth.NewAdaptiveSelector(cliproxyauth.AdaptiveSelectorConfig{Scheduling: cfg.AccountScheduling, SessionAffinity: true}, cliproxyauth.WithAdaptiveClock(now))
			defer selector.Stop()
			manager := cliproxyauth.NewManager(nil, selector, nil)
			manager.SetConfig(cfg)
			manager.RegisterExecutor(NewClaudeExecutor(cfg))
			a := &cliproxyauth.Auth{ID: "synthetic-native-pacing-" + entry, Provider: "claude", Status: cliproxyauth.StatusActive, ProxyURL: "direct", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": server.URL}, Metadata: map[string]any{"quota_snapshot": map[string]any{"profile": map[string]any{"organization": map[string]any{"rate_limit_tier": "default_claude_max_5x"}}}}}
			if _, err := manager.Register(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			model := "claude-3-5-sonnet-20241022"
			registry.GetGlobalRegistry().RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: model}})
			defer registry.GetGlobalRegistry().UnregisterClient(a.ID)
			payload := []byte(strings.ReplaceAll(claudeAttemptRequestJSON, "claude-sonnet-4-6", model))
			if entry == "thinking-clear" || entry == "reject-unknown-edit" {
				edit := `{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}`
				if entry == "reject-unknown-edit" {
					edit = `{"edits":[{"type":"future_generation_edit"}]}`
				}
				payload = []byte(`{"context_management":` + edit + `,` + string(payload[1:]))
			}
			req := cliproxyexecutor.Request{Model: model, Payload: payload}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), OriginalRequest: payload, Headers: http.Header{"X-Claude-Code-Session-Id": []string{"synthetic-native-root"}}}
			invoke := func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				switch entry {
				case "count":
					_, err := manager.ExecuteCount(ctx, []string{"claude"}, req, opts)
					return err
				case "stream", "stream-after-admission-deadline":
					stream, err := manager.ExecuteStream(ctx, []string{"claude"}, req, opts)
					if err != nil {
						return err
					}
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							return chunk.Err
						}
					}
					return nil
				default:
					_, err := manager.Execute(ctx, []string{"claude"}, req, opts)
					return err
				}
			}
			if err := invoke(); err == nil || sends.Load() != 0 {
				t.Fatal("zero credit reached native HTTP", err)
			}
			clock.Add(int64(30 * time.Minute))
			if entry == "reject-unknown-edit" {
				if err := invoke(); err == nil || sends.Load() != 0 {
					t.Fatal("unknown estimate must reject without panic or HTTP send", err)
				}
				latest, _ := manager.GetByID(a.ID)
				if latest.Failed != 0 || latest.Success != 0 || latest.Metadata[cliproxyauth.FirstProductionAtMetadataKey] != nil {
					t.Fatal("local refusal mutated result/anchor accounting")
				}
				return
			}
			started := time.Now()
			if err := invoke(); err != nil {
				t.Fatal(err)
			}
			if entry == "stream-after-admission-deadline" && time.Since(started) < time.Second {
				t.Fatal("stream fixture did not cross admission deadline")
			}
			if sends.Load() != 1 {
				t.Fatalf("native send count=%d", sends.Load())
			}
			latest, ok := manager.GetByID(a.ID)
			if !ok {
				t.Fatal("native auth disappeared")
			}
			if entry == "count" && latest.Metadata[cliproxyauth.FirstProductionAtMetadataKey] != nil {
				t.Fatal("CountTokens stamped first production")
			}
			store, err := cliproxyauth.NewWarmupPacingFileStore(cfg.AuthDir)
			if err != nil {
				t.Fatal(err)
			}
			loaded := cliproxyauth.NewWarmupPacer(store, now)
			d, err := loaded.PeekConfigured(a.ID, cliproxyauth.WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
			wantTokens := int64(17)
			if entry == "count" {
				wantTokens = 0
			}
			if err != nil || d.DayRequests != 1 || d.Tokens != wantTokens || d.PendingTokens != 0 {
				t.Fatalf("durable native settlement: %+v %v", d, err)
			}
		})
	}
}
