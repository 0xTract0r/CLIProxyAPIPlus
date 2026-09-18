package usage

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestWarmupPacingSafetyRunsWithStatisticsDisabled(t *testing.T) {
	old := StatisticsEnabled()
	SetStatisticsEnabled(false)
	t.Cleanup(func() {
		SetStatisticsEnabled(old)
		coreauth.RegisterAccountPacingUsageSink(nil)
		coreauth.RegisterAccountBillableTokenSink(nil)
	})
	calls := 0
	oldCalls := 0
	coreauth.RegisterAccountPacingUsageSink(func(ctx context.Context, provider, id string, tokens int64) bool {
		calls++
		if provider != "claude" || id != "synthetic-history" || tokens != 0 {
			t.Fatalf("unexpected safety event: %q %q %d", provider, id, tokens)
		}
		return true
	})
	coreauth.RegisterAccountBillableTokenSink(func(string, int) { oldCalls++ })
	s := NewRequestStatistics()
	p := &LoggerPlugin{stats: s}
	p.HandleUsage(context.Background(), coreusage.Record{Provider: "claude", AuthID: "synthetic-history"})
	if calls != 1 || oldCalls != 0 || s.totalRequests != 0 {
		t.Fatalf("off reporting lost safety or changed reports: %d/%d/%d", calls, oldCalls, s.totalRequests)
	}
	SetStatisticsEnabled(true)
	p.HandleUsage(context.Background(), coreusage.Record{Provider: "claude", AuthID: "synthetic-history"})
	if calls != 2 || oldCalls != 0 || s.totalRequests != 1 {
		t.Fatal("enabled report double charged or disappeared")
	}
}
