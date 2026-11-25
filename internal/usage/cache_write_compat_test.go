package usage

import (
	"testing"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestOrdinaryUsageStatisticsTreatsCacheWriteAliasesAsOneBucket(t *testing.T) {
	for _, tc := range []coreusage.Detail{
		{CacheWriteTokens: 10}, {CacheCreationTokens: 10}, {CacheWriteTokens: 10, CacheCreationTokens: 10}, {CacheWriteTokens: 90, CacheCreationTokens: 10},
	} {
		tc.InputTokens, tc.OutputTokens, tc.CacheReadTokens, tc.TotalTokens = 6, 2, 4, 8
		stats, err := normaliseDetail(tc)
		if err != nil || stats.CacheWriteTokens != 10 || stats.BillableTokens != 22 {
			t.Errorf("legacy=%d canonical=%d got %v, %v", tc.CacheWriteTokens, tc.CacheCreationTokens, stats, err)
		}
	}
}
