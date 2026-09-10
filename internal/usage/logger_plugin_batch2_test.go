package usage

import "testing"

// TestSchedulerBillableTokensExcludesCacheReads proves the token count fed into the
// adaptive warm-up token budget (harden P3) excludes cache-READ tokens (ITPM
// semantics) while keeping cache-WRITE tokens, unlike billableTokenCount which adds
// cache reads for cost accounting.
func TestSchedulerBillableTokensExcludesCacheReads(t *testing.T) {
	cases := []struct {
		name   string
		tokens TokenStats
		want   int64
	}{
		{
			name:   "excludes cache reads, keeps cache writes",
			tokens: TokenStats{TotalTokens: 1000, CacheReadTokens: 500, CacheWriteTokens: 100},
			want:   1100,
		},
		{
			name:   "derives total from parts when TotalTokens is zero, drops cache reads",
			tokens: TokenStats{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 0, CacheReadTokens: 30},
			want:   150,
		},
		{
			name:   "empty is zero",
			tokens: TokenStats{},
			want:   0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := schedulerBillableTokens(tc.tokens)
			if got != tc.want {
				t.Fatalf("schedulerBillableTokens(%+v) = %d, want %d", tc.tokens, got, tc.want)
			}
			// Sanity: the cost-accounting count DOES include cache reads, so the two
			// must differ whenever cache reads are present.
			if tc.tokens.CacheReadTokens > 0 && billableTokenCount(tc.tokens) == got {
				t.Fatalf("schedulerBillableTokens must differ from billableTokenCount when cache reads present")
			}
		})
	}
}
