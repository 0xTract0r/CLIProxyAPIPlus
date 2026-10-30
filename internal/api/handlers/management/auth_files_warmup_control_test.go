package management

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAccountSchedulingWarmupControlsContract(t *testing.T) {
	h, m := newAccountSchedulingTestHandler(t, "claude")
	h.cfg.AccountScheduling = config.DefaultAccountSchedulingConfig()
	m.SetConfig(h.cfg)
	a, _ := m.GetByID("acct.json")
	a.SetAccountFirstProductionAt(time.Now().Add(-10 * 24 * time.Hour))
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	patch := func(fields string) map[string]any {
		t.Helper()
		rec := patchAccountScheduling(t, h, fmt.Sprintf(`{"name":"acct.json","auth_index":%q,%s}`, a.Index, fields))
		if rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body.String())
		}
		return decodeSchedulingResponse(t, rec)
	}
	patch(`"rate_scale":2,"limit_overrides":{"rpm":8},"temporary_limits":{"duration_minutes":15,"overrides":{"rpm":10}}`)
	view := patch(`"speed_profile":"high","warmup_mode":"auto","warmup_period_days":30`)
	control := view["warmup_control"].(map[string]any)
	if control["speed_profile"] != "high" || control["source"] != "profile" || control["warmup_period_days"] != float64(30) || len(control["profiles"].([]any)) != 4 {
		t.Fatal("incomplete control projection", control)
	}
	if view["rate_scale"] != float64(1) {
		t.Fatal("profile retained rate scale")
	}
	for _, raw := range []string{`"speed_profile":"extreme","rate_scale":1`, `"speed_profile":"high","warmup_period_days":0`, `"warmup_mode":"invalid"`} {
		rec := patchAccountScheduling(t, h, fmt.Sprintf(`{"name":"acct.json","auth_index":%q,%s}`, a.Index, raw))
		if rec.Code != 400 {
			t.Fatal("invalid patch accepted", rec.Code, rec.Body.String())
		}
		current, _ := m.GetByID(a.ID)
		if got := coreauth.AccountWarmupControlFor(current, time.Now(), h.cfg.AccountScheduling); got.SpeedProfile == nil || *got.SpeedProfile != "high" {
			t.Fatal("rejected patch partially published")
		}
	}
	view = patch(`"rate_scale":0.5`)
	control = view["warmup_control"].(map[string]any)
	if control["speed_profile"] != nil || control["source"] != "legacy" {
		t.Fatal("advanced edit concealed by selected profile", control)
	}
	view = patch(`"speed_profile":"extreme","warmup_mode":"mature"`)
	control = view["warmup_control"].(map[string]any)
	profiles := control["profiles"].([]any)
	first := profiles[0].(map[string]any)
	for _, p := range profiles[1:] {
		row := p.(map[string]any)
		for _, key := range []string{"rpm", "daily_budget", "concurrency", "request_capacity", "max_active_bindings"} {
			if row[key] != first[key] {
				t.Fatal("mature recommendations differ", profiles)
			}
		}
	}
	view = patch(`"warmup_mode":null,"warmup_period_days":null,"speed_profile":null`)
	control = view["warmup_control"].(map[string]any)
	if control["warmup_mode"] != "auto" || control["warmup_period_days"] != nil || control["speed_profile"] != nil {
		t.Fatal("explicit clear lost", control)
	}
	rec := patchAccountScheduling(t, h, `{"name":"acct.json","speed_profile":"low"}`)
	if rec.Code != 400 {
		t.Fatal("new controls accepted ambiguous account target")
	}
}

func TestAccountSchedulingWarmupControlsPersistReload(t *testing.T) {
	dir := t.TempDir()
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	cfg := &config.Config{AuthDir: dir, AccountScheduling: config.DefaultAccountSchedulingConfig()}
	manager := coreauth.NewManager(store, nil, nil)
	manager.SetConfig(cfg)
	a, err := manager.Register(context.Background(), &coreauth.Auth{ID: "warmup-controls.json", Provider: "claude", Metadata: map[string]any{"type": "claude", "access_token": "synthetic-local-only"}})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, authManager: manager}
	rec := patchAccountScheduling(t, h, fmt.Sprintf(`{"name":"warmup-controls.json","auth_index":%q,"speed_profile":"low","warmup_period_days":365,"warmup_mode":"mature"}`, a.Index))
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	reloaded, err := store.List(context.Background())
	if err != nil || len(reloaded) != 1 {
		t.Fatalf("file reload: len=%d err=%v", len(reloaded), err)
	}
	restarted := coreauth.NewManager(store, nil, nil)
	restarted.SetConfig(cfg)
	current, err := restarted.Register(context.Background(), reloaded[0])
	if err != nil {
		t.Fatal(err)
	}
	got := coreauth.AccountWarmupControlFor(current, time.Now(), cfg.AccountScheduling)
	if got.SpeedProfile == nil || *got.SpeedProfile != "low" || got.WarmupPeriodDays == nil || *got.WarmupPeriodDays != 365 || !got.ManualMature {
		t.Fatal("real file reload lost controls", got)
	}
}
