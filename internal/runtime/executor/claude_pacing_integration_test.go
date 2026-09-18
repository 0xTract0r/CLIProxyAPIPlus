package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestClaudePacingManagerNativeHTTP(t *testing.T) {
	for _, entry := range []string{"execute", "stream", "count"} {
		t.Run(entry, func(t *testing.T) {
			var sends atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if entry == "count" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"input_tokens":100}`)
					return
				}
				if entry == "stream" {
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
			req := cliproxyexecutor.Request{Model: model, Payload: payload}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), OriginalRequest: payload, Headers: http.Header{"X-Claude-Code-Session-Id": []string{"synthetic-native-root"}}}
			invoke := func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				switch entry {
				case "count":
					_, err := manager.ExecuteCount(ctx, []string{"claude"}, req, opts)
					return err
				case "stream":
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
			if err := invoke(); err != nil {
				t.Fatal(err)
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
