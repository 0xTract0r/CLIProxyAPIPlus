package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func previewAccountScheduling(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/account-scheduling/preview", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PreviewAuthFileAccountScheduling(c)
	return w
}

type schedulingPreviewHook struct {
	coreauth.NoopHook
	updates atomic.Int32
}

func (h *schedulingPreviewHook) OnAuthUpdated(context.Context, *coreauth.Auth) {
	h.updates.Add(1)
}

func previewTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		out[path] = fmt.Sprintf("%d:%s", info.ModTime().UnixNano(), data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPreviewAccountSchedulingReadOnlyRuntimeAndFiles(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)
	now := time.Now().Add(-time.Hour)
	cfg := &config.Config{AuthDir: t.TempDir(), AccountScheduling: config.DefaultAccountSchedulingConfig()}
	cfg.AccountScheduling.WarmupTrafficPacing.Enabled = true
	selector := coreauth.NewAdaptiveSelector(coreauth.AdaptiveSelectorConfig{Scheduling: cfg.AccountScheduling, SessionAffinity: true}, coreauth.WithAdaptiveClock(func() time.Time { return now }), coreauth.WithAdaptiveRand(func() float64 { return 0.999 }))
	defer selector.Stop()
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(cfg.AuthDir)
	hook := &schedulingPreviewHook{}
	m := coreauth.NewManager(store, selector, hook)
	m.SetConfig(cfg)
	for _, id := range []string{"acct.json", "peer.json"} {
		a := &coreauth.Auth{ID: id, FileName: id, Provider: "claude", ProxyURL: "http://127.0.0.1:9", Success: 3, Metadata: map[string]any{"type": "claude", "access_token": "synthetic-disabled-only", "refresh_disabled": true}}
		age := 10
		if id == "peer.json" {
			age = 70
		}
		a.SetAccountFirstProductionAt(time.Now().Add(-time.Duration(age) * 24 * time.Hour))
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Hour)
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": {"preview-session"}}}
	bound, err := selector.Pick(context.Background(), "claude", "", opts, m.List())
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, authManager: m}
	a, _ := m.GetByID("acct.json")
	beforeAuth := m.List()
	sort.Slice(beforeAuth, func(i, j int) bool { return beforeAuth[i].ID < beforeAuth[j].ID })
	beforeFiles := previewTree(t, cfg.AuthDir)
	beforePacing := m.WarmupTrafficPacingSnapshot(a.ID)
	if beforePacing.RequestBalance == nil || *beforePacing.RequestBalance <= 0 {
		t.Fatal("fixture must have a funded live pacing ledger", beforePacing)
	}
	for _, fields := range []string{``, `,"speed_profile":"high"`, `,"warmup_mode":"mature"`, `,"warmup_period_days":30`, `,"temporary_limits":{"duration_minutes":15,"overrides":{"rpm":8}}`, `,"speed_profile":"high","warmup_period_days":0`} {
		w := previewAccountScheduling(t, h, fmt.Sprintf(`{"name":"acct.json","auth_index":%q%s}`, a.Index, fields))
		if strings.Contains(fields, `:0`) {
			if w.Code != 400 {
				t.Fatal(w.Code, w.Body.String())
			}
		} else if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	// Manager.List来自map，列表顺序不属于只读不变量；逐账号完整字段仍须保持一致。
	afterAuth := m.List()
	sort.Slice(afterAuth, func(i, j int) bool { return afterAuth[i].ID < afterAuth[j].ID })
	if !reflect.DeepEqual(beforeAuth, afterAuth) {
		t.Fatal("preview changed auth/UpdatedAt or usage")
	}
	if !reflect.DeepEqual(beforeFiles, previewTree(t, cfg.AuthDir)) {
		t.Fatal("preview changed durable files or modification times")
	}
	if !reflect.DeepEqual(beforePacing, m.WarmupTrafficPacingSnapshot(a.ID)) {
		t.Fatal("preview changed pacing balance or leases")
	}
	if hook.updates.Load() != 0 || h.managedHeaderScheduler != nil || h.reloadGeneration != 0 {
		t.Fatal("preview published an auth/config update or scheduled header refresh")
	}
	afterBound, err := selector.Pick(context.Background(), "claude", "", opts, m.List())
	if err != nil || afterBound.ID != bound.ID {
		t.Fatal("preview changed session binding", afterBound, err)
	}
}

func TestPreviewAccountSchedulingCandidatePolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		age        int
		initial    map[string]any
		fields     string
		mature     bool
		period     float64
		healthCap  bool
		clearLayer bool
	}{
		{name: "identity only", age: 10},
		{name: "mature to auto", age: 10, initial: map[string]any{"warmup_mode": "mature"}, fields: `,"warmup_mode":"auto"`},
		{name: "shorter period", age: 45, fields: `,"warmup_period_days":30`, mature: true, period: 30},
		{name: "longer period", age: 70, fields: `,"warmup_period_days":120`, period: 120},
		{name: "health gate", age: 45, initial: map[string]any{"warmup_health_stage_cap": 0}, fields: `,"warmup_mode":"mature","speed_profile":"extreme"`, healthCap: true},
		{name: "profile clears overrides", age: 10, initial: map[string]any{"rate_scale": 0.5, "limit_overrides": map[string]int{"rpm": 9}}, fields: `,"speed_profile":"high"`, clearLayer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, m := newAccountSchedulingTestHandler(t, "claude")
			h.cfg.AccountScheduling = config.DefaultAccountSchedulingConfig()
			m.SetConfig(h.cfg)
			a, _ := m.GetByID("acct.json")
			a.SetAccountFirstProductionAt(time.Now().Add(-time.Duration(tc.age) * 24 * time.Hour))
			obj := a.Metadata[coreauth.AccountSchedulingMetadataKey].(map[string]any)
			for key, value := range tc.initial {
				obj[key] = value
			}
			if tc.clearLayer {
				if err := coreauth.ApplyAccountLimitPatch(a, h.cfg.AccountScheduling, nil, []byte(`{"duration_minutes":15,"overrides":{"rpm":10}}`), time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.Update(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			before, _ := m.GetByID(a.ID)
			w := previewAccountScheduling(t, h, fmt.Sprintf(`{"name":"acct.json","auth_index":%q%s}`, a.Index, tc.fields))
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var response map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			candidate := response["account_scheduling"].(map[string]any)
			current := response["current_account_scheduling"].(map[string]any)
			if response["preview"] != true || response["auth_index"] != a.Index || candidate["warmup"].(map[string]any)["mature"] != tc.mature {
				t.Fatal("invalid candidate projection", response)
			}
			if len(candidate) != len(current) || candidate["first_production_at"] != current["first_production_at"] {
				t.Fatal("candidate/current schema or true anchor diverged", response)
			}
			for key := range current {
				if _, ok := candidate[key]; !ok {
					t.Fatal("candidate/current field sets differ", key)
				}
			}
			for _, forbidden := range []string{"warmup_traffic_pacing", "sessions_total", "sessions_active", "sessions_closed"} {
				if _, exists := candidate[forbidden]; exists {
					t.Fatal("preview advertised runtime observations", forbidden)
				}
			}
			if tc.period != 0 && candidate["warmup_control"].(map[string]any)["warmup_period_days"] != tc.period {
				t.Fatal("period missing", candidate)
			}
			if tc.healthCap && candidate["effective_limits"].(map[string]any)["rpm"].(float64) > 3 {
				t.Fatal("manual maturity bypassed health gate", candidate)
			}
			if tc.clearLayer {
				policy := candidate["limit_policy"].(map[string]any)
				if policy["temporary_status"] != "none" || policy["fields"].(map[string]any)["rpm"].(map[string]any)["fixed"] != nil {
					t.Fatal("profile did not replace old numeric layers", candidate)
				}
				if current["limit_policy"].(map[string]any)["temporary_status"] != "active" {
					t.Fatal("current policy was overwritten by preview", current)
				}
			}
			after, _ := m.GetByID(a.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("preview mutated saved auth")
			}
			if tc.fields == "" && !reflect.DeepEqual(candidate, current) {
				t.Fatal("no-op preview is not identical to current policy")
			}
		})
	}
}

func TestPreviewAccountSchedulingRejectsInvalidAndReadOnlyTargets(t *testing.T) {
	h, m := newAccountSchedulingTestHandler(t, "claude")
	h.cfg.AccountScheduling = config.DefaultAccountSchedulingConfig()
	a, _ := m.GetByID("acct.json")
	for _, body := range []string{`{`, `{"name":"acct.json"}`, `{"auth_index":"x"}`, `{"name":"acct.json","auth_index":123}`, fmt.Sprintf(`{"name":"acct.json","auth_index":%q,"speed_profile":"high","rate_scale":2}`, a.Index), fmt.Sprintf(`{"name":"acct.json","auth_index":%q,"warmup_mode":"invalid"}`, a.Index), fmt.Sprintf(`{"name":"acct.json","auth_index":%q,"limit_overrides":{"concurrency":99}}`, a.Index)} {
		if w := previewAccountScheduling(t, h, body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := previewAccountScheduling(t, h, `{"name":"acct.json","auth_index":"wrong"}`); w.Code != 404 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		name, provider, attr, value string
		status                      int
	}{
		{"non-Claude", "codex", "", "", 400},
		{"plugin", "claude", coreauth.AttributePluginVirtual, "true", 409},
		{"static", "claude", "source", "config:claude[synthetic]", 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, m := newAccountSchedulingTestHandler(t, tc.provider)
			a, _ := m.GetByID("acct.json")
			if tc.attr != "" {
				a.Attributes[tc.attr] = tc.value
				a.Attributes["api_key"] = "synthetic-local-key"
				if _, err := m.Update(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			w := previewAccountScheduling(t, h, fmt.Sprintf(`{"name":"acct.json","auth_index":%q}`, a.Index))
			if w.Code != tc.status {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	var missing *Handler
	if w := previewAccountScheduling(t, missing, `{"name":"acct.json","auth_index":"x"}`); w.Code != 503 {
		t.Fatal(w.Code)
	}
	if w := previewAccountScheduling(t, &Handler{}, `{"name":"acct.json","auth_index":"x"}`); w.Code != 503 {
		t.Fatal(w.Code)
	}
	peer := &coreauth.Auth{ID: "peer", FileName: "acct.json", Provider: "claude"}
	registered, err := m.Register(context.Background(), peer)
	if err != nil {
		t.Fatal(err)
	}
	w := previewAccountScheduling(t, h, fmt.Sprintf(`{"name":"acct.json","auth_index":%q}`, registered.Index))
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(registered.Index)) {
		t.Fatal(w.Code, w.Body.String())
	}
}
