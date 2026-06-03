package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestToolPrefixDisabled(t *testing.T) {
	var a *Auth
	if a.ToolPrefixDisabled() {
		t.Error("nil auth should return false")
	}

	a = &Auth{}
	if a.ToolPrefixDisabled() {
		t.Error("empty auth should return false")
	}

	a = &Auth{Metadata: map[string]any{"tool_prefix_disabled": true}}
	if !a.ToolPrefixDisabled() {
		t.Error("should return true when set to true")
	}

	a = &Auth{Metadata: map[string]any{"tool_prefix_disabled": "true"}}
	if !a.ToolPrefixDisabled() {
		t.Error("should return true when set to string 'true'")
	}

	a = &Auth{Metadata: map[string]any{"tool-prefix-disabled": true}}
	if !a.ToolPrefixDisabled() {
		t.Error("should return true with kebab-case key")
	}

	a = &Auth{Metadata: map[string]any{"tool_prefix_disabled": false}}
	if a.ToolPrefixDisabled() {
		t.Error("should return false when set to false")
	}
}

func TestRefreshDisabled(t *testing.T) {
	var a *Auth
	if a.RefreshDisabled() {
		t.Error("nil auth should not disable refresh")
	}

	a = &Auth{Metadata: map[string]any{"refresh_disabled": true}}
	if !a.RefreshDisabled() {
		t.Error("refresh_disabled=true should disable refresh")
	}

	a = &Auth{Metadata: map[string]any{"disable_refresh": "true"}}
	if !a.RefreshDisabled() {
		t.Error("disable_refresh=true should disable refresh")
	}

	a = &Auth{Metadata: map[string]any{"refresh_enabled": false}}
	if !a.RefreshDisabled() {
		t.Error("refresh_enabled=false should disable refresh")
	}

	a = &Auth{Metadata: map[string]any{
		"account_settings": map[string]any{"refresh_enabled": false},
	}}
	if !a.RefreshDisabled() {
		t.Error("account_settings.refresh_enabled=false should disable refresh")
	}

	a = &Auth{Metadata: map[string]any{
		"account_settings": map[string]string{"auto_refresh": "false"},
	}}
	if !a.RefreshDisabled() {
		t.Error("account_settings.auto_refresh=false should disable refresh")
	}

	a = &Auth{Attributes: map[string]string{"refresh_disabled": "true"}}
	if !a.RefreshDisabled() {
		t.Error("attribute refresh_disabled=true should disable refresh")
	}

	a = &Auth{Metadata: map[string]any{"refresh_disabled": false, "refresh_enabled": true}}
	if a.RefreshDisabled() {
		t.Error("explicit false disabled keys and true enabled keys should keep refresh enabled")
	}

	a = &Auth{Metadata: map[string]any{"refresh_status": "reauth_required"}}
	if !a.RefreshDisabled() {
		t.Error("refresh_status=reauth_required should disable refresh")
	}

	a = &Auth{Metadata: map[string]any{"reauth_required": true}}
	if !a.RefreshDisabled() {
		t.Error("reauth_required=true should disable refresh")
	}
}

func TestSubscriptionPlanTypeFromNestedClaudeProfile(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want string
	}{
		{
			name: "canonical metadata wins",
			auth: &Auth{Metadata: map[string]any{
				"plan_type": "max",
				"quota_snapshot": map[string]any{
					"profile": map[string]any{"subscription": map[string]any{"has_claude_pro": true}},
				},
			}},
			want: "max",
		},
		{
			name: "nested max boolean",
			auth: &Auth{Metadata: map[string]any{
				"quota_snapshot": map[string]any{
					"profile": map[string]any{"subscription": map[string]any{"has_claude_max": true}},
				},
			}},
			want: "max",
		},
		{
			name: "nested subscription tier string",
			auth: &Auth{Metadata: map[string]any{
				"quota_snapshot": map[string]any{
					"profile": map[string]any{"subscription": map[string]any{"subscription_tier": "Claude Pro"}},
				},
			}},
			want: "Claude Pro",
		},
		{
			name: "production claude max profile ignores subscription status fields",
			auth: &Auth{Metadata: map[string]any{
				"quota_snapshot": map[string]any{
					"profile": map[string]any{
						"account": map[string]any{"has_claude_max": true},
						"organization": map[string]any{
							"rate_limit_tier":         "default_claude_max_20x",
							"subscription_created_at": "2026-03-31T17:41:42Z",
							"subscription_status":     "canceled",
						},
					},
				},
			}},
			want: "max",
		},
		{
			name: "attributes fallback",
			auth: &Auth{Attributes: map[string]string{"plan_type": "plus"}},
			want: "plus",
		},
		{
			name: "reauth required preserves last known premium plan",
			auth: &Auth{
				Metadata: map[string]any{
					"quota_refresh_status": "reauth_required",
					"plan_type":            "max",
					"quota_snapshot": map[string]any{
						"profile": map[string]any{"subscription": map[string]any{"has_claude_max": true}},
					},
				},
				Attributes: map[string]string{"plan_type": "max"},
			},
			want: "max",
		},
		{
			name: "legacy unauthorized quota error preserves last known premium plan",
			auth: &Auth{
				Metadata: map[string]any{
					"quota_refresh_status": "error",
					"quota_refresh_error":  "quota endpoint returned 401: invalid token",
					"plan_type":            "max",
				},
				Attributes: map[string]string{"plan_type": "max"},
			},
			want: "max",
		},
		{
			name: "refresh disabled imported plan remains usable",
			auth: &Auth{Metadata: map[string]any{
				"refresh_disabled": true,
				"plan_type":        "max",
			}},
			want: "max",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.auth.SubscriptionPlanType(); got != tt.want {
				t.Fatalf("SubscriptionPlanType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnsureIndexUsesCredentialIdentity(t *testing.T) {
	t.Parallel()

	geminiAuth := &Auth{
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
			"source":  "config:gemini[abc123]",
		},
	}
	compatAuth := &Auth{
		Provider: "bohe",
		Attributes: map[string]string{
			"api_key":      "shared-key",
			"compat_name":  "bohe",
			"provider_key": "bohe",
			"source":       "config:bohe[def456]",
		},
	}
	geminiAltBase := &Auth{
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key":  "shared-key",
			"base_url": "https://alt.example.com",
			"source":   "config:gemini[ghi789]",
		},
	}
	geminiDuplicate := &Auth{
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
			"source":  "config:gemini[abc123-1]",
		},
	}

	geminiIndex := geminiAuth.EnsureIndex()
	compatIndex := compatAuth.EnsureIndex()
	altBaseIndex := geminiAltBase.EnsureIndex()
	duplicateIndex := geminiDuplicate.EnsureIndex()

	if geminiIndex == "" {
		t.Fatal("gemini index should not be empty")
	}
	if compatIndex == "" {
		t.Fatal("compat index should not be empty")
	}
	if altBaseIndex == "" {
		t.Fatal("alt base index should not be empty")
	}
	if duplicateIndex == "" {
		t.Fatal("duplicate index should not be empty")
	}
	if geminiIndex == compatIndex {
		t.Fatalf("shared api key produced duplicate auth_index %q", geminiIndex)
	}
	if geminiIndex == altBaseIndex {
		t.Fatalf("same provider/key with different base_url produced duplicate auth_index %q", geminiIndex)
	}
	if geminiIndex != duplicateIndex {
		t.Fatalf("same provider/key with different source should share auth_index, got %q vs %q", geminiIndex, duplicateIndex)
	}
}

func TestEnsureIndexUsesOAuthTypeAndAbsolutePath(t *testing.T) {
	t.Parallel()

	wd, errWd := os.Getwd()
	if errWd != nil {
		t.Fatalf("os.Getwd returned error: %v", errWd)
	}

	relPath := "test-oauth.json"
	absPath := filepath.Join(wd, relPath)
	expectedSeed := "gemini:" + filepath.Clean(absPath)
	expectedIndex := stableAuthIndex(expectedSeed)

	a := &Auth{
		Provider: "gemini-cli",
		Attributes: map[string]string{
			"path": relPath,
		},
		Metadata: map[string]any{
			"type": "gemini",
		},
	}

	got := a.EnsureIndex()
	if got == "" {
		t.Fatal("auth index should not be empty")
	}
	if got != expectedIndex {
		t.Fatalf("auth index = %q, want %q", got, expectedIndex)
	}
}

func TestRecentRequestsSnapshotEmptyReturnsTwentyBuckets(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).In(time.Local)
	a := &Auth{}

	got := a.RecentRequestsSnapshot(now)
	if len(got) != recentRequestBucketCount {
		t.Fatalf("len = %d, want %d", len(got), recentRequestBucketCount)
	}

	currentBucketID := now.Unix() / recentRequestBucketSeconds
	baseBucketID := currentBucketID - int64(recentRequestBucketCount-1)
	for i, bucket := range got {
		if bucket.Success != 0 || bucket.Failed != 0 {
			t.Fatalf("bucket[%d] counts = %d/%d, want 0/0", i, bucket.Success, bucket.Failed)
		}
		if strings.TrimSpace(bucket.Time) == "" {
			t.Fatalf("bucket[%d] time label is empty", i)
		}
		expectedBucketID := baseBucketID + int64(i)
		start := time.Unix(expectedBucketID*recentRequestBucketSeconds, 0).In(time.Local)
		end := start.Add(10 * time.Minute)
		expected := start.Format("15:04") + "-" + end.Format("15:04")
		if bucket.Time != expected {
			t.Fatalf("bucket[%d] time = %q, want %q", i, bucket.Time, expected)
		}
	}
}

func TestRecentRequestsSnapshotIncludesCounts(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).In(time.Local)
	a := &Auth{}

	a.recordRecentRequest(now, true)
	a.recordRecentRequest(now, false)

	got := a.RecentRequestsSnapshot(now)
	if len(got) != recentRequestBucketCount {
		t.Fatalf("len = %d, want %d", len(got), recentRequestBucketCount)
	}

	newest := got[len(got)-1]
	if newest.Success != 1 || newest.Failed != 1 {
		t.Fatalf("newest bucket = success=%d failed=%d, want 1/1", newest.Success, newest.Failed)
	}
}

func TestRecentRequestsSnapshotBucketAdvanceMovesCounts(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).In(time.Local)
	next := now.Add(10 * time.Minute)
	a := &Auth{}

	a.recordRecentRequest(now, true)
	a.recordRecentRequest(next, false)

	got := a.RecentRequestsSnapshot(next)
	if len(got) != recentRequestBucketCount {
		t.Fatalf("len = %d, want %d", len(got), recentRequestBucketCount)
	}

	secondNewest := got[len(got)-2]
	newest := got[len(got)-1]
	if secondNewest.Success != 1 || secondNewest.Failed != 0 {
		t.Fatalf("second newest bucket = success=%d failed=%d, want 1/0", secondNewest.Success, secondNewest.Failed)
	}
	if newest.Success != 0 || newest.Failed != 1 {
		t.Fatalf("newest bucket = success=%d failed=%d, want 0/1", newest.Success, newest.Failed)
	}
}
