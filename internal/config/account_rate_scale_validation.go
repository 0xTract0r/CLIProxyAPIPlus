package config

import (
	"fmt"
	"math"
)

// ValidateAccountRateScale rejects nonfinite values and products that cannot
// fit the integer counters used by execution, pacing and management.
func ValidateAccountRateScale(scale float64, c AccountSchedulingConfig) error {
	if scale <= 0 || math.IsNaN(scale) || math.IsInf(scale, 0) {
		return fmt.Errorf("account-scheduling.rate-scale must be positive and finite")
	}
	largest := max(1, c.MatureLimits.RPMLimit, c.MatureLimits.ConcurrencyLimit, c.MatureLimits.Burst, c.MatureLimits.TokenDailyBudget)
	for _, stage := range c.WarmupCurve {
		largest = max(largest, stage.RPMLimit, stage.ConcurrencyLimit, stage.DailyBudget, stage.TokenDailyBudget)
	}
	if scale >= float64(math.MaxInt)/float64(largest) {
		return fmt.Errorf("account-scheduling.rate-scale would overflow effective limits")
	}
	return nil
}
