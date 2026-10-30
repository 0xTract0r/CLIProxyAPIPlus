package auth

import (
	"reflect"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestSchedulingPreviewCloneDoesNotLatchLiveTemporaryExpiry(t *testing.T) {
	now := time.Now()
	cfg := internalconfig.DefaultAccountSchedulingConfig()
	a := &Auth{Metadata: map[string]any{"other": map[string]any{"nested": []any{map[string]any{"value": "original"}}}}}
	a.SetAccountFirstProductionAt(now.Add(-10 * 24 * time.Hour))
	if err := ApplyAccountLimitPatch(a, cfg, nil, []byte(`{"duration_minutes":15,"overrides":{"rpm":8}}`), now); err != nil {
		t.Fatal(err)
	}
	preview := a.CloneForSchedulingPreview()
	if preview.temporaryLimits == a.temporaryLimits {
		t.Fatal("preview shares live expiry latch")
	}
	view := AccountLimitPolicyFor(preview, now.Add(16*time.Minute), cfg)
	if view.TemporaryStatus != "expired" || !preview.temporaryLimits.expired.Load() {
		t.Fatal("preview failed to resolve its own future expiry", view)
	}
	if a.temporaryLimits.expired.Load() || AccountLimitPolicyFor(a, now, cfg).TemporaryStatus != "active" {
		t.Fatal("speculative future expiry changed live lifetime")
	}
	preview.Metadata["other"].(map[string]any)["nested"].([]any)[0].(map[string]any)["value"] = "draft"
	if got := a.Metadata["other"].(map[string]any)["nested"].([]any)[0].(map[string]any)["value"]; got != "original" {
		t.Fatal("preview shares non-policy metadata")
	}
	if err := ApplyAccountLimitPatch(preview, cfg, []byte(`{"rpm":10}`), []byte(`null`), now); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(a.Metadata[AccountSchedulingMetadataKey], preview.Metadata[AccountSchedulingMetadataKey]) || a.temporaryLimits == nil {
		t.Fatal("preview failed to isolate scheduling metadata")
	}
	var absent *Auth
	if absent.CloneForSchedulingPreview() != nil {
		t.Fatal("nil clone must remain nil")
	}
}
