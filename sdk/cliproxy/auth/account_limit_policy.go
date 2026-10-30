package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sync/atomic"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type AccountLimitField struct {
	Baseline  float64 `json:"baseline"`
	Fixed     *int    `json:"fixed"`
	Temporary *int    `json:"temporary"`
	Effective float64 `json:"effective"`
	Source    string  `json:"source"`
	Min       int     `json:"min"`
	Max       int     `json:"max"`
	ceiling   float64
}

type AccountLimitPolicy struct {
	Fields             map[string]AccountLimitField `json:"fields"`
	TemporaryStatus    string                       `json:"temporary_status"`
	TemporaryIssuedAt  *string                      `json:"temporary_issued_at"`
	TemporaryExpiresAt *string                      `json:"temporary_expires_at"`
	TemporaryEndReason *string                      `json:"temporary_end_reason"`
	RestrictedReasons  []string                     `json:"restricted_reasons"`
}

type temporaryAccountLimits struct {
	ID              string         `json:"id"`
	IssuedAt        string         `json:"issued_at"`
	ExpiresAt       string         `json:"expires_at"`
	DurationMinutes int            `json:"duration_minutes"`
	Overrides       map[string]int `json:"overrides"`
	Ended           bool           `json:"ended,omitempty"`
}

// Clones share this process-local lifetime, never the persistent metadata map.
// A disk reload has no monotonic anchor and cannot reactivate a temporary boost.
type temporaryLimitRuntime struct {
	id       string
	started  time.Time
	duration time.Duration
	expired  atomic.Bool
}

func limitBounds(cfg internalconfig.AccountSchedulingConfig) map[string][2]int {
	return map[string][2]int{"rpm": {1, 60}, "concurrency": {1, 4}, "request_capacity": {max(1, cfg.WarmupTrafficPacing.MinAdmissionRequests), 32}, "max_active_bindings": {1, 8}}
}

func storedLimitOverrides(a *Auth) map[string]int {
	var out map[string]int
	if a != nil {
		raw, _ := accountSchedulingRawValue(a.Metadata, "limit_overrides")
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func storedTemporaryLimits(a *Auth) *temporaryAccountLimits {
	if a == nil {
		return nil
	}
	raw, ok := accountSchedulingRawValue(a.Metadata, "temporary_limits")
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out temporaryAccountLimits
	if json.Unmarshal(b, &out) != nil || out.ID == "" {
		return nil
	}
	return &out
}

func temporaryLimitsState(a *Auth, t *temporaryAccountLimits, now time.Time) (string, string) {
	if t == nil {
		return "none", ""
	}
	if t.Ended {
		return "ended", "ended_by_operator"
	}
	issued, e1 := time.Parse(time.RFC3339Nano, t.IssuedAt)
	expires, e2 := time.Parse(time.RFC3339Nano, t.ExpiresAt)
	if e1 != nil || e2 != nil || !expires.After(issued) {
		return "ended", "invalid_temporary_policy"
	}
	r := a.temporaryLimits
	if r == nil || r.id != t.ID {
		return "restart_expired", "process_restarted"
	}
	if r.expired.Load() {
		return "expired", "duration_elapsed"
	}
	if !now.Before(expires) || now.Before(issued) || time.Since(r.started) >= r.duration {
		r.expired.Store(true)
		return "expired", "duration_elapsed"
	}
	return "active", ""
}

// AccountLimitPolicyFor is shared by selectors, final admission and management.
// Input bounds constrain explicit edits only, never historical baseline limits.
func AccountLimitPolicyFor(a *Auth, now time.Time, cfg internalconfig.AccountSchedulingConfig) AccountLimitPolicy {
	status := AccountWarmupStatusFor(a, now, cfg)
	scale := AccountRateScale(a, cfg)
	baseline := map[string]float64{
		"rpm":                 scaleLimitRPM(float64(status.RPMLimit), scale),
		"concurrency":         float64(scaleLimitInt(status.ConcurrencyLimit, scale)),
		"request_capacity":    float64(cfg.WarmupTrafficPacing.RequestBurst),
		"max_active_bindings": float64(cfg.WarmupTrafficPacing.MaxActiveBindings),
	}
	out := AccountLimitPolicy{Fields: make(map[string]AccountLimitField), TemporaryStatus: "none", RestrictedReasons: []string{}}
	tmp := storedTemporaryLimits(a)
	state, reason := temporaryLimitsState(a, tmp, now)
	out.TemporaryStatus = state
	if tmp != nil {
		out.TemporaryIssuedAt = &tmp.IssuedAt
		out.TemporaryExpiresAt = &tmp.ExpiresAt
	}
	if reason != "" {
		out.TemporaryEndReason = &reason
	}
	fixed := storedLimitOverrides(a)
	_, hasHealthCap := AccountHealthStageCap(a)
	healthRestricted := cfg.HealthGate.Enabled && (hasHealthCap || AccountInDistress(a, cfg, now))
	if healthRestricted {
		out.RestrictedReasons = append(out.RestrictedReasons, "health_gate")
	}
	if a != nil && (a.Disabled || a.Status == StatusDisabled || a.AutoQuarantined || a.Unavailable) {
		out.RestrictedReasons = append(out.RestrictedReasons, "account_unavailable")
	}
	for key, bounds := range limitBounds(cfg) {
		f := AccountLimitField{Baseline: baseline[key], Effective: baseline[key], Source: "baseline", Min: bounds[0], Max: bounds[1]}
		if v, ok := fixed[key]; ok && v >= bounds[0] && v <= bounds[1] {
			f.Fixed = &v
			f.Effective = float64(v)
			f.Source = "fixed"
		}
		if state == "active" {
			if v, ok := tmp.Overrides[key]; ok && v >= bounds[0] && v <= bounds[1] {
				f.Temporary = &v
				f.Effective = float64(v)
				f.Source = "temporary"
			}
		}
		if healthRestricted && f.Effective > f.Baseline {
			f.Effective = f.Baseline
			f.Source = "restricted"
		}
		f.ceiling = f.Effective
		if key == "rpm" && !status.Mature {
			if pace := accountPacingRPMMultiplier(a, now); pace < 1 {
				f.Effective *= pace
				f.Source = "restricted"
				out.RestrictedReasons = append(out.RestrictedReasons, "quota_pacing")
			}
		}
		out.Fields[key] = f
	}
	return out
}

// ApplyAccountLimitPatch validates the complete new combination before writing
// only scheduling metadata on the caller-owned clone.
func ApplyAccountLimitPatch(a *Auth, cfg internalconfig.AccountSchedulingConfig, fixedRaw, temporaryRaw json.RawMessage, now time.Time) error {
	if err := internalconfig.ValidateAccountRateScale(AccountRateScale(a, cfg), cfg); err != nil {
		return err
	}
	bounds := limitBounds(cfg)
	fixed := storedLimitOverrides(a)
	if fixed == nil {
		fixed = make(map[string]int)
	}
	decode := func(raw json.RawMessage, allowNull bool) (map[string]*int, error) {
		var values map[string]json.RawMessage
		if json.Unmarshal(raw, &values) != nil || values == nil {
			return nil, errors.New("limit overrides must be an object")
		}
		out := make(map[string]*int)
		for key, value := range values {
			b, ok := bounds[key]
			if !ok {
				return nil, errors.New("unknown limit field")
			}
			if string(value) == "null" && allowNull {
				out[key] = nil
				continue
			}
			var v int
			if json.Unmarshal(value, &v) != nil || v < b[0] || v > b[1] {
				return nil, errors.New("limit outside engineering bounds")
			}
			out[key] = &v
		}
		return out, nil
	}
	if len(fixedRaw) > 0 {
		if string(fixedRaw) == "null" {
			fixed = map[string]int{}
		} else {
			values, err := decode(fixedRaw, true)
			if err != nil {
				return err
			}
			for key, v := range values {
				if v == nil {
					delete(fixed, key)
				} else {
					fixed[key] = *v
				}
			}
		}
	}
	tmp := storedTemporaryLimits(a)
	var runtime *temporaryLimitRuntime
	if len(temporaryRaw) > 0 {
		if string(temporaryRaw) == "null" {
			if tmp != nil {
				tmp.Ended = true
			}
		} else {
			var payload struct {
				DurationMinutes int             `json:"duration_minutes"`
				Overrides       json.RawMessage `json:"overrides"`
			}
			if json.Unmarshal(temporaryRaw, &payload) != nil || (payload.DurationMinutes != 5 && payload.DurationMinutes != 15 && payload.DurationMinutes != 30) {
				return errors.New("temporary duration must be 5, 15 or 30 minutes")
			}
			values, err := decode(payload.Overrides, false)
			if err != nil || len(values) == 0 {
				return errors.New("temporary overrides must contain valid integer limits")
			}
			var id [16]byte
			if _, err := rand.Read(id[:]); err != nil {
				return errors.New("cannot issue temporary policy")
			}
			duration := time.Duration(payload.DurationMinutes) * time.Minute
			tmp = &temporaryAccountLimits{ID: hex.EncodeToString(id[:]), IssuedAt: now.UTC().Format(time.RFC3339Nano), ExpiresAt: now.Add(duration).UTC().Format(time.RFC3339Nano), DurationMinutes: payload.DurationMinutes, Overrides: map[string]int{}}
			for key, v := range values {
				tmp.Overrides[key] = *v
			}
			runtime = &temporaryLimitRuntime{id: tmp.ID, started: time.Now(), duration: duration}
		}
	}
	if a.Metadata == nil {
		a.Metadata = map[string]any{}
	}
	if len(fixedRaw) > 0 {
		setAccountSchedulingValue(a.Metadata, "limit_overrides", fixed)
	}
	if len(temporaryRaw) > 0 && tmp != nil {
		b, _ := json.Marshal(tmp)
		var stored map[string]any
		_ = json.Unmarshal(b, &stored)
		setAccountSchedulingValue(a.Metadata, "temporary_limits", stored)
		if runtime != nil {
			a.temporaryLimits = runtime
		}
	}
	return nil
}

func finitePositive(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

// Generic refresh/watcher updates do not own operator policy. Preserve the
// current policy and its monotonic proof; only UpdateAccountScheduling edits it.
func preserveAccountLimitPolicy(a, existing *Auth) {
	if a == nil || existing == nil {
		return
	}
	for _, key := range []string{"limit_overrides", "temporary_limits"} {
		if value, ok := accountSchedulingRawValue(existing.Clone().Metadata, key); ok {
			if a.Metadata == nil {
				a.Metadata = map[string]any{}
			}
			setAccountSchedulingValue(a.Metadata, key, value)
		}
	}
	if tmp := storedTemporaryLimits(a); tmp != nil && existing.temporaryLimits != nil && tmp.ID == existing.temporaryLimits.id {
		a.temporaryLimits = existing.temporaryLimits
	}
}
