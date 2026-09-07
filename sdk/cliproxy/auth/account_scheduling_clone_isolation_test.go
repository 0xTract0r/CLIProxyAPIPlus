package auth

import (
	"reflect"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// TestClone_AccountSchedulingMapIsDeepCopied is the fast, deterministic unit guard
// for the HIGH concurrency fix (the race reproduction lives in the management
// package's -race test). Before the fix Auth.Clone shallow-copied Metadata, so a
// clone shared the nested account_scheduling map[string]any by reference with the
// original; an in-place write to one (setAccountSchedulingValue, as MarkResult and
// the operator PATCH both do) was visible in the other and could race a concurrent
// read. This asserts each clone now owns an INDEPENDENT deep copy of that object:
// the two maps are distinct backing stores, and mutating either side never leaks
// into the other. Every other Metadata top-level value keeps shallow-copy
// semantics, which the surrounding suite already covers.
func TestClone_AccountSchedulingMapIsDeepCopied(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	original := &Auth{ID: "acct", Provider: "claude", Metadata: map[string]any{}}
	original.SetAccountFirstProductionAt(now.Add(-30 * 24 * time.Hour))
	original.SetAccountRateScale(0.5)
	original.setHealthStageCap(2)

	clone := original.Clone()

	// The nested account_scheduling objects must be distinct backing stores.
	origMap, ok := original.Metadata[AccountSchedulingMetadataKey].(map[string]any)
	if !ok {
		t.Fatalf("original account_scheduling is not map[string]any: %#v", original.Metadata[AccountSchedulingMetadataKey])
	}
	cloneMap, ok := clone.Metadata[AccountSchedulingMetadataKey].(map[string]any)
	if !ok {
		t.Fatalf("clone account_scheduling is not map[string]any: %#v", clone.Metadata[AccountSchedulingMetadataKey])
	}
	if reflect.ValueOf(origMap).Pointer() == reflect.ValueOf(cloneMap).Pointer() {
		t.Fatal("clone shares the same account_scheduling map backing store as the original (shallow copy)")
	}

	// Mutating the clone must not leak into the original.
	clone.setHealthStageCap(0)
	if cap, ok := AccountHealthStageCap(original); !ok || cap != 2 {
		t.Fatalf("original health cap after clone mutation = (%d,%v), want (2,true) [clone write leaked]", cap, ok)
	}

	// Mutating the original must not leak into the clone.
	original.SetAccountRateScale(9.0)
	if cloneScale := AccountRateScale(clone, internalconfig.AccountSchedulingConfig{}); cloneScale != 0.5 {
		t.Fatalf("clone rate_scale after original mutation = %v, want 0.5 [original write leaked]", cloneScale)
	}
}
