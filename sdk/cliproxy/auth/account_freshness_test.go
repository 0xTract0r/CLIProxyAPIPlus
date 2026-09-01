package auth

import (
	"testing"
	"time"
)

var accountFreshnessTestAnchor = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestEnsureAuthFirstProductionAt_MintsOnceThenPreserves(t *testing.T) {
	auth := &Auth{}

	anchor, minted := EnsureAuthFirstProductionAt(auth, accountFreshnessTestAnchor)
	if !minted {
		t.Fatalf("first call: minted = false, want true")
	}
	if !anchor.Equal(accountFreshnessTestAnchor) {
		t.Fatalf("first call: anchor = %v, want %v", anchor, accountFreshnessTestAnchor)
	}
	stored, ok := auth.Metadata[FirstProductionAtMetadataKey].(string)
	if !ok {
		t.Fatalf("first call: Metadata[%q] not stored as string, got %#v", FirstProductionAtMetadataKey, auth.Metadata[FirstProductionAtMetadataKey])
	}
	if want := accountFreshnessTestAnchor.Format(time.RFC3339); stored != want {
		t.Fatalf("first call: stored value = %q, want %q", stored, want)
	}

	// A later call, even hours afterward, must not move the anchor
	// (append-only: set once, never overwritten).
	later := accountFreshnessTestAnchor.Add(48 * time.Hour)
	anchor2, minted2 := EnsureAuthFirstProductionAt(auth, later)
	if minted2 {
		t.Fatalf("second call: minted = true, want false (anchor already set)")
	}
	if !anchor2.Equal(accountFreshnessTestAnchor) {
		t.Fatalf("second call: anchor = %v, want unchanged %v", anchor2, accountFreshnessTestAnchor)
	}
	if stored2 := auth.Metadata[FirstProductionAtMetadataKey]; stored2 != stored {
		t.Fatalf("second call: Metadata[%q] changed to %#v, want unchanged %q", FirstProductionAtMetadataKey, stored2, stored)
	}
}

func TestEnsureAuthFirstProductionAt_PreservesOtherMetadataKeys(t *testing.T) {
	auth := &Auth{
		Metadata: map[string]any{
			"quota_snapshot": map[string]any{"foo": "bar"},
			"farm_enrolled":  true,
			"note":           "AC-14",
		},
	}

	if _, minted := EnsureAuthFirstProductionAt(auth, accountFreshnessTestAnchor); !minted {
		t.Fatalf("minted = false, want true")
	}

	if len(auth.Metadata) != 4 {
		t.Fatalf("Metadata has %d keys after Ensure, want 4 (3 pre-existing + first_production_at): %#v", len(auth.Metadata), auth.Metadata)
	}
	if _, ok := auth.Metadata["quota_snapshot"]; !ok {
		t.Fatalf("quota_snapshot key was dropped")
	}
	if v, _ := auth.Metadata["farm_enrolled"].(bool); !v {
		t.Fatalf("farm_enrolled key was dropped or mutated")
	}
	if v, _ := auth.Metadata["note"].(string); v != "AC-14" {
		t.Fatalf("note key was dropped or mutated, got %#v", auth.Metadata["note"])
	}
	if _, ok := auth.Metadata[FirstProductionAtMetadataKey]; !ok {
		t.Fatalf("first_production_at was not written")
	}
}

func TestEnsureAuthFirstProductionAt_NilAuth(t *testing.T) {
	anchor, minted := EnsureAuthFirstProductionAt(nil, accountFreshnessTestAnchor)
	if minted {
		t.Fatalf("minted = true for nil auth, want false")
	}
	if !anchor.IsZero() {
		t.Fatalf("anchor = %v for nil auth, want zero value", anchor)
	}
}

func TestEnsureAuthFirstProductionAt_OverwritesCorruptStoredValue(t *testing.T) {
	auth := &Auth{Metadata: map[string]any{FirstProductionAtMetadataKey: "not-a-timestamp"}}

	anchor, minted := EnsureAuthFirstProductionAt(auth, accountFreshnessTestAnchor)
	if !minted {
		t.Fatalf("minted = false, want true (corrupt stored value must not count as an existing anchor)")
	}
	if !anchor.Equal(accountFreshnessTestAnchor) {
		t.Fatalf("anchor = %v, want %v", anchor, accountFreshnessTestAnchor)
	}
	stored, _ := auth.Metadata[FirstProductionAtMetadataKey].(string)
	if want := accountFreshnessTestAnchor.Format(time.RFC3339); stored != want {
		t.Fatalf("stored value = %q, want corrupt value replaced with %q", stored, want)
	}
}

func TestAuthFirstProductionAt(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want time.Time
		ok   bool
	}{
		{name: "nil auth", auth: nil, ok: false},
		{name: "nil metadata", auth: &Auth{}, ok: false},
		{name: "missing key", auth: &Auth{Metadata: map[string]any{"other": "x"}}, ok: false},
		{
			name: "valid RFC3339 string",
			auth: &Auth{Metadata: map[string]any{FirstProductionAtMetadataKey: "2026-01-01T00:00:00Z"}},
			want: accountFreshnessTestAnchor,
			ok:   true,
		},
		{
			name: "valid time.Time value",
			auth: &Auth{Metadata: map[string]any{FirstProductionAtMetadataKey: accountFreshnessTestAnchor}},
			want: accountFreshnessTestAnchor,
			ok:   true,
		},
		{name: "zero time.Time value", auth: &Auth{Metadata: map[string]any{FirstProductionAtMetadataKey: time.Time{}}}, ok: false},
		{name: "blank string", auth: &Auth{Metadata: map[string]any{FirstProductionAtMetadataKey: "   "}}, ok: false},
		{name: "unparseable string", auth: &Auth{Metadata: map[string]any{FirstProductionAtMetadataKey: "yesterday"}}, ok: false},
		{name: "wrong type", auth: &Auth{Metadata: map[string]any{FirstProductionAtMetadataKey: 12345}}, ok: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := AuthFirstProductionAt(tc.auth)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if tc.ok && !got.Equal(tc.want) {
				t.Fatalf("anchor = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAccountAge(t *testing.T) {
	t.Run("no anchor recorded", func(t *testing.T) {
		age, ok := AccountAge(&Auth{}, accountFreshnessTestAnchor)
		if ok {
			t.Fatalf("ok = true with no anchor, want false")
		}
		if age != 0 {
			t.Fatalf("age = %v with no anchor, want 0", age)
		}
	})

	t.Run("90 days old", func(t *testing.T) {
		auth := &Auth{Metadata: map[string]any{FirstProductionAtMetadataKey: accountFreshnessTestAnchor.Format(time.RFC3339)}}
		now := accountFreshnessTestAnchor.Add(90 * 24 * time.Hour)
		age, ok := AccountAge(auth, now)
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		if want := 90 * 24 * time.Hour; age != want {
			t.Fatalf("age = %v, want %v", age, want)
		}
	})

	t.Run("clock skew before anchor is clamped to zero", func(t *testing.T) {
		auth := &Auth{Metadata: map[string]any{FirstProductionAtMetadataKey: accountFreshnessTestAnchor.Format(time.RFC3339)}}
		now := accountFreshnessTestAnchor.Add(-1 * time.Hour)
		age, ok := AccountAge(auth, now)
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		if age != 0 {
			t.Fatalf("age = %v, want 0 (clamped)", age)
		}
	})
}

func TestAccountAgeDays(t *testing.T) {
	auth := &Auth{Metadata: map[string]any{FirstProductionAtMetadataKey: accountFreshnessTestAnchor.Format(time.RFC3339)}}

	tests := []struct {
		name string
		now  time.Time
		want int
	}{
		{name: "same instant", now: accountFreshnessTestAnchor, want: 0},
		{name: "exactly 7 days", now: accountFreshnessTestAnchor.Add(7 * 24 * time.Hour), want: 7},
		{
			name: "truncates partial day (6d23h59m)",
			now:  accountFreshnessTestAnchor.Add(6*24*time.Hour + 23*time.Hour + 59*time.Minute),
			want: 6,
		},
		{name: "60 days (mature boundary)", now: accountFreshnessTestAnchor.Add(60 * 24 * time.Hour), want: 60},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := AccountAgeDays(auth, tc.now)
			if !ok {
				t.Fatalf("ok = false, want true")
			}
			if got != tc.want {
				t.Fatalf("days = %d, want %d", got, tc.want)
			}
		})
	}

	t.Run("no anchor recorded", func(t *testing.T) {
		got, ok := AccountAgeDays(&Auth{}, accountFreshnessTestAnchor)
		if ok {
			t.Fatalf("ok = true with no anchor, want false")
		}
		if got != 0 {
			t.Fatalf("days = %d with no anchor, want 0", got)
		}
	})
}
