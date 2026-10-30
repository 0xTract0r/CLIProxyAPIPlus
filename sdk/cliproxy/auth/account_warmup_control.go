package auth

import (
	"encoding/json"
	"errors"
	"math"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

var accountSpeedProfiles = []string{"low", "medium", "high", "extreme"}

// AccountWarmupControl is the authoritative editor projection. Recommendations
// resolve through the same policy and budget helpers as live admission.
type AccountWarmupControl struct {
	SpeedProfile        *string               `json:"speed_profile"`
	WarmupMode          string                `json:"warmup_mode"`
	WarmupPeriodDays    *int                  `json:"warmup_period_days"`
	EffectivePeriodDays *int                  `json:"effective_period_days"`
	Source              string                `json:"source"`
	ManualMature        bool                  `json:"manual_mature"`
	Profiles            []AccountSpeedProfile `json:"profiles"`
}

type AccountSpeedProfile struct {
	ID                string  `json:"id"`
	RPM               float64 `json:"rpm"`
	DailyBudget       int     `json:"daily_budget"`
	Concurrency       int     `json:"concurrency"`
	RequestCapacity   int     `json:"request_capacity"`
	MaxActiveBindings int     `json:"max_active_bindings"`
}

func accountSpeedProfile(a *Auth) string {
	if a == nil {
		return ""
	}
	value := accountSchedulingString(a.Metadata, "speed_profile")
	for _, id := range accountSpeedProfiles {
		if value == id {
			return id
		}
	}
	return ""
}

func accountSpeedMultiplier(a *Auth) float64 {
	switch accountSpeedProfile(a) {
	case "low":
		return 0.5
	case "high":
		return 1.5
	case "extreme":
		return 2
	default:
		return 1
	}
}

func accountManualMature(a *Auth) bool {
	return a != nil && accountSchedulingString(a.Metadata, "warmup_mode") == "mature"
}

func accountWarmupPeriod(a *Auth) *int {
	if a == nil {
		return nil
	}
	raw, ok := accountSchedulingRawValue(a.Metadata, "warmup_period_days")
	if !ok {
		return nil
	}
	data, err := json.Marshal(raw)
	var days int
	if err != nil || json.Unmarshal(data, &days) != nil || days < 1 || days > 365 {
		return nil
	}
	return &days
}

// An explicit finite period also bounds an originally unbounded last stage.
// Its virtual end extends that stage by the previous stage's width (at least
// one day for a one-stage curve). Original metadata/curve are never rewritten.
func accountWarmupTimeline(a *Auth, now time.Time, cfg internalconfig.AccountSchedulingConfig) (int, bool, internalconfig.AccountSchedulingConfig) {
	age, anchored := AccountAgeDays(a, now)
	period := accountWarmupPeriod(a)
	if period != nil && len(cfg.WarmupCurve) > 0 {
		cfg.WarmupCurve = append([]internalconfig.AccountWarmupStage(nil), cfg.WarmupCurve...)
		last := len(cfg.WarmupCurve) - 1
		end := cfg.WarmupCurve[last].MaxAgeDays
		if end == 0 {
			width := 1
			if last > 0 {
				width = max(1, cfg.WarmupCurve[last-1].MaxAgeDays-cfg.WarmupCurve[last-1].MinAgeDays)
			}
			end = cfg.WarmupCurve[last].MinAgeDays + width
			cfg.WarmupCurve[last].MaxAgeDays = end
		}
		elapsed, _ := AccountAge(a, now)
		periodDuration := time.Duration(*period) * 24 * time.Hour
		if elapsed >= periodDuration {
			age = end
		} else {
			// Scale elapsed time before rounding to stage days, so a 30-day
			// period reaches the original day-7 boundary after 3.5 real days.
			age = min(end-1, int(float64(elapsed)/float64(periodDuration)*float64(end)))
		}
	}
	return age, anchored, cfg
}

func accountWarmupBaseStatus(a *Auth, now time.Time, cfg internalconfig.AccountSchedulingConfig) AccountWarmupStatus {
	age, anchored, timeline := accountWarmupTimeline(a, now, cfg)
	if accountManualMature(a) {
		// An explicit calendar override never clears health history or upstream gates.
		if cfg.HealthGate.Enabled && len(timeline.WarmupCurve) > 0 {
			cap, capped := AccountHealthStageCap(a)
			if !capped && AccountInDistress(a, cfg, now) {
				cap, capped = 0, true
			}
			if capped {
				cap = max(0, min(cap, len(timeline.WarmupCurve)-1))
				return AccountWarmupStageForAge(timeline.WarmupCurve[cap].MinAgeDays, true, timeline.WarmupCurve, cfg.MatureLimits)
			}
		}
		return matureAccountWarmupStatus(cfg.MatureLimits)
	}
	age, anchored = effectiveWarmupAge(a, timeline, age, anchored)
	return AccountWarmupStageForAge(age, anchored, timeline.WarmupCurve, cfg.MatureLimits)
}

func accountHealthRestricted(a *Auth, now time.Time, cfg internalconfig.AccountSchedulingConfig) bool {
	_, capped := AccountHealthStageCap(a)
	return cfg.HealthGate.Enabled && (capped || AccountInDistress(a, cfg, now))
}

func accountRequestDailyBudget(a *Auth, now time.Time, cfg internalconfig.AccountSchedulingConfig) int {
	status := AccountWarmupStatusFor(a, now, cfg)
	legacy := scaleLimitInt(status.DailyBudget, AccountRateScale(a, cfg))
	if status.Mature || accountSpeedProfile(a) == "" {
		return legacy
	}
	budget := scaleLimitInt(status.DailyBudget, accountSpeedMultiplier(a))
	if accountHealthRestricted(a, now, cfg) {
		budget = min(budget, legacy)
	}
	return budget
}

func accountLimitBaseline(a *Auth, now time.Time, cfg internalconfig.AccountSchedulingConfig, status AccountWarmupStatus) map[string]float64 {
	scale := AccountRateScale(a, cfg)
	baseline := map[string]float64{
		"rpm":                 scaleLimitRPM(float64(status.RPMLimit), scale),
		"concurrency":         float64(scaleLimitInt(status.ConcurrencyLimit, scale)),
		"request_capacity":    float64(cfg.WarmupTrafficPacing.RequestBurst),
		"max_active_bindings": float64(cfg.WarmupTrafficPacing.MaxActiveBindings),
	}
	if status.Mature || accountSpeedProfile(a) == "" {
		return baseline
	}
	multiplier := accountSpeedMultiplier(a)
	groups := 1
	switch accountSpeedProfile(a) {
	case "high":
		groups = 2
	case "extreme":
		groups = 4
	}
	profiled := map[string]float64{
		"rpm":                 float64(scaleLimitInt(status.RPMLimit, multiplier)),
		"concurrency":         float64(max(1, min(4, int(math.Ceil(float64(status.ConcurrencyLimit)*multiplier))))),
		"request_capacity":    float64(max(max(1, cfg.WarmupTrafficPacing.MinAdmissionRequests), scaleLimitInt(cfg.WarmupTrafficPacing.RequestBurst, multiplier))),
		"max_active_bindings": float64(max(1, min(8, cfg.WarmupTrafficPacing.MaxActiveBindings*groups))),
	}
	if accountHealthRestricted(a, now, cfg) {
		for key := range profiled {
			profiled[key] = min(profiled[key], baseline[key])
		}
	}
	return profiled
}

func AccountWarmupControlFor(a *Auth, now time.Time, cfg internalconfig.AccountSchedulingConfig) AccountWarmupControl {
	out := AccountWarmupControl{WarmupMode: "auto", Source: "legacy", Profiles: []AccountSpeedProfile{}, WarmupPeriodDays: accountWarmupPeriod(a)}
	if accountManualMature(a) {
		out.WarmupMode, out.ManualMature = "mature", true
	}
	if profile := accountSpeedProfile(a); profile != "" {
		out.SpeedProfile, out.Source = &profile, "profile"
	}
	out.EffectivePeriodDays = out.WarmupPeriodDays
	if out.EffectivePeriodDays == nil && len(cfg.WarmupCurve) > 0 {
		if end := cfg.WarmupCurve[len(cfg.WarmupCurve)-1].MaxAgeDays; end > 0 {
			out.EffectivePeriodDays = &end
		}
	}
	for _, id := range accountSpeedProfiles {
		candidate := &Auth{}
		if a != nil {
			candidate = a.Clone()
		}
		setAccountSpeedProfile(candidate, id)
		fields := AccountLimitPolicyFor(candidate, now, cfg).Fields
		out.Profiles = append(out.Profiles, AccountSpeedProfile{ID: id, RPM: fields["rpm"].Effective, DailyBudget: accountRequestDailyBudget(candidate, now, cfg), Concurrency: int(fields["concurrency"].Effective), RequestCapacity: int(fields["request_capacity"].Effective), MaxActiveBindings: int(fields["max_active_bindings"].Effective)})
	}
	return out
}

func setAccountSpeedProfile(a *Auth, profile string) {
	if a.Metadata == nil {
		a.Metadata = map[string]any{}
	}
	setAccountSchedulingValue(a.Metadata, "speed_profile", profile)
	a.ClearAccountRateScale()
	setAccountSchedulingValue(a.Metadata, "limit_overrides", map[string]int{})
	setAccountSchedulingValue(a.Metadata, "temporary_limits", nil)
	a.temporaryLimits = nil
}

// ApplyAccountWarmupControlPatch validates all new controls before mutation.
// Presence (including null) is distinct from omission; mixed control dialects
// are rejected so a selected profile can never conceal a numeric override.
func ApplyAccountWarmupControlPatch(a *Auth, req map[string]json.RawMessage) error {
	profileRaw, profilePresent := req["speed_profile"]
	if profilePresent {
		for _, key := range []string{"rate_scale", "limit_overrides", "temporary_limits"} {
			if _, ok := req[key]; ok {
				return errors.New("speed_profile cannot be mixed with legacy limit controls")
			}
		}
	}
	var profile, mode string
	var period int
	if profilePresent && string(profileRaw) != "null" {
		if json.Unmarshal(profileRaw, &profile) != nil {
			return errors.New("invalid speed_profile")
		}
		valid := false
		for _, id := range accountSpeedProfiles {
			valid = valid || profile == id
		}
		if !valid {
			return errors.New("speed_profile must be low, medium, high, extreme or null")
		}
	}
	modeRaw, modePresent := req["warmup_mode"]
	if modePresent && string(modeRaw) != "null" {
		if json.Unmarshal(modeRaw, &mode) != nil || (mode != "auto" && mode != "mature") {
			return errors.New("warmup_mode must be auto, mature or null")
		}
	}
	periodRaw, periodPresent := req["warmup_period_days"]
	if periodPresent && string(periodRaw) != "null" {
		if json.Unmarshal(periodRaw, &period) != nil || period < 1 || period > 365 {
			return errors.New("warmup_period_days must be an integer from 1 to 365 or null")
		}
	}
	if a.Metadata == nil {
		a.Metadata = map[string]any{}
	}
	if profilePresent {
		if profile != "" {
			setAccountSpeedProfile(a, profile)
		} else {
			setAccountSchedulingValue(a.Metadata, "speed_profile", nil)
		}
	} else {
		for _, key := range []string{"rate_scale", "limit_overrides", "temporary_limits"} {
			if _, ok := req[key]; ok {
				setAccountSchedulingValue(a.Metadata, "speed_profile", nil)
				break
			}
		}
	}
	if modePresent {
		if mode == "" {
			mode = "auto"
		}
		setAccountSchedulingValue(a.Metadata, "warmup_mode", mode)
	}
	if periodPresent {
		if period == 0 {
			setAccountSchedulingValue(a.Metadata, "warmup_period_days", nil)
		} else {
			setAccountSchedulingValue(a.Metadata, "warmup_period_days", period)
		}
	}
	return nil
}
