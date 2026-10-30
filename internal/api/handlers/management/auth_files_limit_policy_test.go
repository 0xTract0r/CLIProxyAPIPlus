package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAccountSchedulingOAuthFilePolicySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	cfg := &config.Config{AuthDir: dir, AccountScheduling: config.DefaultAccountSchedulingConfig()}
	m := coreauth.NewManager(store, nil, nil)
	m.SetConfig(cfg)
	a := &coreauth.Auth{ID: "oauth-fixture.json", Provider: "claude", Metadata: map[string]any{"type": "claude", "access_token": "synthetic-local-only"}}
	registered, err := m.Register(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if registered.Attributes["path"] == "" {
		t.Fatal("policy persistence changed the existing store path publication")
	}
	h := &Handler{cfg: cfg, authManager: m}
	body := fmt.Sprintf(`{"name":"oauth-fixture.json","auth_index":%q,"limit_overrides":{"rpm":8},"temporary_limits":{"duration_minutes":15,"overrides":{"rpm":10}}}`, registered.Index)
	rec := patchAccountScheduling(t, h, body)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, "oauth-fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	if json.Unmarshal(data, &disk) != nil || disk[coreauth.AccountSchedulingMetadataKey] == nil {
		t.Fatal("operator policy missing from real auth file")
	}
	reloaded, err := store.List(context.Background())
	if err != nil || len(reloaded) != 1 {
		t.Fatalf("real file reload: count=%d err=%v", len(reloaded), err)
	}
	restarted := coreauth.NewManager(store, nil, nil)
	restarted.SetConfig(cfg)
	current, err := restarted.Register(context.Background(), reloaded[0])
	if err != nil {
		t.Fatal(err)
	}
	policy := coreauth.AccountLimitPolicyFor(current, time.Now(), cfg.AccountScheduling)
	if policy.TemporaryStatus != "restart_expired" || policy.Fields["rpm"].Effective != 8 {
		t.Fatal("restart lost fixed policy or restored temporary boost", policy)
	}
}

func TestAccountSchedulingStaticConfigLimitsReadOnly(t *testing.T) {
	h, m := newAccountSchedulingTestHandler(t, "claude")
	a, _ := m.GetByID("acct.json")
	a.Attributes["api_key"] = "synthetic-local-key"
	a.Attributes["source"] = "config:claude[synthetic]"
	if _, err := m.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	rec := patchAccountScheduling(t, h, fmt.Sprintf(`{"name":"acct.json","auth_index":%q,"rate_scale":2,"limit_overrides":{"rpm":8}}`, a.Index))
	if rec.Code != 409 {
		t.Fatal("static account accepted persistent limit edit", rec.Code, rec.Body.String())
	}
	updated, _ := m.GetByID(a.ID)
	if coreauth.AccountRateScale(updated, h.cfg.AccountScheduling) != 1 {
		t.Fatal("409 partially changed old fields")
	}
	if h.buildAccountSchedulingView(updated)["limit_policy"] != nil {
		t.Fatal("static account advertised editable policy")
	}
	if rec = patchAccountScheduling(t, h, `{"name":"acct.json","rate_scale":2}`); rec.Code != 200 {
		t.Fatal("legacy rate-scale editing regressed", rec.Body.String())
	}
}

func TestAccountSchedulingLimitPatchAndExactLookup(t *testing.T) {
	h, m := newAccountSchedulingTestHandler(t, "claude")
	h.cfg.AccountScheduling = config.DefaultAccountSchedulingConfig()
	m.SetConfig(h.cfg)
	a, _ := m.GetByID("acct.json")
	for _, body := range []string{
		`{"name":"acct.json","limit_overrides":{"rpm":8}}`,
		`{"name":"acct.json","auth_index":"wrong","rate_scale":2}`,
		fmt.Sprintf(`{"name":"acct.json","auth_index":%q,"rate_scale":2,"limit_overrides":{"concurrency":999}}`, a.Index),
	} {
		rec := patchAccountScheduling(t, h, body)
		if rec.Code < 400 {
			t.Fatal("invalid or mismatched patch accepted", rec.Body.String())
		}
		current, _ := m.GetByID(a.ID)
		if coreauth.AccountRateScale(current, h.cfg.AccountScheduling) != 1 {
			t.Fatal("invalid patch partially applied")
		}
	}
	body := fmt.Sprintf(`{"name":"acct.json","auth_index":%q,"limit_overrides":{"rpm":8,"concurrency":2},"temporary_limits":{"duration_minutes":15,"overrides":{"rpm":10}}}`, a.Index)
	rec := patchAccountScheduling(t, h, body)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	view := decodeSchedulingResponse(t, rec)
	policy := view["limit_policy"].(map[string]any)
	if policy["temporary_status"] != "active" {
		t.Fatal("temporary policy not reflected", policy)
	}
	rec = patchAccountScheduling(t, h, fmt.Sprintf(`{"name":"acct.json","auth_index":%q,"limit_overrides":{"rpm":null},"temporary_limits":null}`, a.Index))
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	view = decodeSchedulingResponse(t, rec)
	policy = view["limit_policy"].(map[string]any)
	fields := policy["fields"].(map[string]any)
	if fields["rpm"].(map[string]any)["fixed"] != nil || fields["concurrency"].(map[string]any)["fixed"] != float64(2) {
		t.Fatal("field clear erased another field", fields)
	}
	for _, index := range []string{"", "wrong", a.Index} {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest("GET", "/v0/management/auth-files/warmup-pacing?name=acct.json&auth_index="+index, nil)
		h.GetAuthFileWarmupPacing(ctx)
		if index != a.Index {
			if w.Code < 400 {
				t.Fatal("narrow read fell back to name")
			}
			continue
		}
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var result map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &result)
		if result["auth_index"] != a.Index || result["account_scheduling"] == nil {
			t.Fatal("invalid narrow projection", result)
		}
		if h.managedHeaderScheduler != nil {
			t.Fatal("narrow read scheduled background header synchronization")
		}
	}
}
