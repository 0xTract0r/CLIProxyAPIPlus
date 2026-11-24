package auth

import (
	"fmt"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// 只读单授权配置与已给定选择元数据，不注册调度器/执行器，不发起或重发请求。
func TestOrdinaryHomeSingleCredentialRetryConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name            string
		global          int
		metadata        map[string]any
		hasAggregate    bool
		aggregate, want int
	}{
		{"aggregate_overrides_credential_zero", 0, map[string]any{"request_retry": float64(0)}, true, 2, 2},
		{"explicit_aggregate_zero_is_authoritative", 3, map[string]any{"request_retry": float64(3)}, true, 0, 0},
		{"absent_aggregate_uses_credential_override", 3, map[string]any{"request_retry": float64(1)}, false, 99, 1},
		{"absent_aggregate_and_override_use_global", 3, nil, false, 99, 3},
	} {
		for _, maxWait := range []time.Duration{0, 30 * time.Millisecond} {
			t.Run(fmt.Sprintf("%s/wait=%s", tc.name, maxWait), func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
				manager.SetRetryConfig(tc.global, maxWait, 1)
				auth := &Auth{ID: "ordinary-single-config", Provider: "codex", Metadata: tc.metadata}
				selection := &HomeDispatchSelection{hasRequestRetry: tc.hasAggregate, requestRetry: tc.aggregate}
				limit := -1
				manager.observeHomeRetryLimit(auth, selection, &limit)
				if limit != tc.want {
					t.Fatalf("observed retry budget = %d, want %d", limit, tc.want)
				}
				global, _, storedWait := manager.retrySettings()
				if global != tc.global || storedWait != maxWait {
					t.Fatalf("configuration changed: global=%d wait=%s", global, storedWait)
				}
				if manager.homeRetryAllowed(-1, limit) || manager.homeRetryAllowed(limit, limit) {
					t.Fatal("negative or exhausted request budget was allowed")
				}
				if limit > 0 && !manager.homeRetryAllowed(limit-1, limit) {
					t.Fatal("last configured budget position was rejected")
				}
			})
		}
	}
}
