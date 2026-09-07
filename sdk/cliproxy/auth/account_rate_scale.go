package auth

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// AccountRateScale returns the effective per-account rate multiplier applied to
// an account's DERIVED rate ceilings (rpm / burst / concurrency / daily budget)
// -- never to its selection weight (design §8.3, spec.md "per-账号安全测试速率乘子").
//
// Resolution order:
//  1. the per-account metadata override account_scheduling.rate_scale (dual-read
//     also honors a legacy bare rate_scale key), when present and > 0;
//  2. the config default account-scheduling.rate-scale, when > 0;
//  3. 1.0 (no effect).
//
// A non-positive or unparseable value at any layer is skipped in favor of the
// next, so the returned multiplier is always > 0 and 1.0 is always a safe no-op.
// The multiplier is meant to be applied AFTER tier/warm-up derivation, so it
// scales whatever ceiling the account currently sits at (warming or mature) and
// stays independent of which account the selector picks.
func AccountRateScale(a *Auth, cfg internalconfig.AccountSchedulingConfig) float64 {
	if a != nil {
		if raw, ok := accountSchedulingRawValue(a.Metadata, accountSchedulingRateScaleKey); ok {
			if v, ok := parseRateScaleValue(raw); ok {
				return v
			}
		}
	}
	if cfg.RateScale > 0 {
		return cfg.RateScale
	}
	return 1.0
}

// parseRateScaleValue coerces a raw metadata value into a positive rate-scale
// multiplier. It accepts the shapes a persisted-and-reloaded auth.Metadata value
// can take (float64 from JSON, json.Number when a decoder uses UseNumber, an
// int/int64 from an in-memory test fixture, or a numeric string from a
// hand-edited dev auth file) and rejects anything non-numeric or <= 0.
func parseRateScaleValue(raw any) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return positiveFloat(v)
	case float32:
		return positiveFloat(float64(v))
	case int:
		return positiveFloat(float64(v))
	case int64:
		return positiveFloat(float64(v))
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return positiveFloat(f)
		}
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return positiveFloat(f)
		}
	}
	return 0, false
}

func positiveFloat(f float64) (float64, bool) {
	if f > 0 {
		return f, true
	}
	return 0, false
}

// ParseRateScaleValue exposes parseRateScaleValue for write-time validation at
// the management tier/rate_scale endpoint: it coerces a raw JSON value (float64,
// json.Number, int, or a numeric string) into a positive rate-scale multiplier
// and reports ok=false for anything non-numeric or <= 0. The management handler
// uses ok=false to return a 4xx instead of persisting an invalid multiplier.
func ParseRateScaleValue(raw any) (float64, bool) {
	return parseRateScaleValue(raw)
}

// SetAccountRateScale persists a per-account rate_scale multiplier into the
// namespaced account_scheduling object (design §8.3/§8.5). value MUST be > 0
// (validate via ParseRateScaleValue first); this writer does not re-validate.
// Metadata is initialized when absent. AccountRateScale then returns this value
// in preference to the config default.
func (a *Auth) SetAccountRateScale(value float64) {
	if a == nil {
		return
	}
	if a.Metadata == nil {
		a.Metadata = make(map[string]any)
	}
	setAccountSchedulingValue(a.Metadata, accountSchedulingRateScaleKey, value)
}

// ClearAccountRateScale removes rate_scale from BOTH the namespaced
// account_scheduling object and the legacy bare top-level key (see
// clearAccountSchedulingValue). AccountRateScale then falls back to the config
// default account-scheduling.rate-scale, else 1.0 (no throttle).
func (a *Auth) ClearAccountRateScale() {
	if a == nil || a.Metadata == nil {
		return
	}
	clearAccountSchedulingValue(a.Metadata, accountSchedulingRateScaleKey)
}

// scaleLimitRPM scales a derived rpm ceiling by scale. A non-positive rpm
// (no ceiling / unset) is preserved unchanged; scale is guaranteed > 0 by
// AccountRateScale, so a positive rpm always stays positive.
func scaleLimitRPM(rpm, scale float64) float64 {
	if rpm <= 0 {
		return rpm
	}
	return rpm * scale
}

// scaleLimitInt scales a derived integer ceiling (burst / concurrency / daily
// budget) by scale, rounding to the nearest whole unit and never dropping a
// positive limit below 1 -- so a fractional scale can throttle an account but can
// never wedge it at a permanent zero ceiling. A non-positive limit (0 = unbounded
// / unset, e.g. a mature account's daily budget) is preserved unchanged.
func scaleLimitInt(limit int, scale float64) int {
	if limit <= 0 {
		return limit
	}
	scaled := int(math.Round(float64(limit) * scale))
	if scaled < 1 {
		scaled = 1
	}
	return scaled
}
