package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"path/filepath"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
)

// WarmupTrafficPacingSnapshot is an observation, not a request admission result.
// Nil numbers mean unknown; counts include live, unsent reservations.
type WarmupTrafficPacingSnapshot struct {
	Status                     string   `json:"status"`
	Reason                     string   `json:"reason"`
	ObservedAt                 string   `json:"observed_at"`
	RequestBalance             *float64 `json:"request_balance"`
	RequestCapacity            *int     `json:"request_capacity"`
	MinAdmissionRequests       *int     `json:"min_admission_requests"`
	RefillPerHour              *float64 `json:"refill_per_hour"`
	AdmissionBalanceETASeconds *float64 `json:"admission_balance_eta_seconds"`
	Rolling24hRequests         *int     `json:"rolling_24h_requests"`
	DailyRequestBudget         *int     `json:"daily_request_budget"`
	Rolling60sRequests         *int     `json:"rolling_60s_requests"`
	RPMLimit                   *int     `json:"rpm_limit"`
	ActiveBindingGroups        *int     `json:"active_binding_groups"`
	MaxActiveBindingGroups     *int     `json:"max_active_binding_groups"`
	ActiveBindingIdleSeconds   *int     `json:"active_binding_idle_seconds"`
	Inflight                   *int     `json:"inflight"`
	ConcurrencyLimit           *int     `json:"concurrency_limit"`
	PendingRequests            *int     `json:"pending_requests"`
	BlockingReasons            []string `json:"blocking_reasons"`
}

func pacingSnapshotStatus(now time.Time, status, reason string) WarmupTrafficPacingSnapshot {
	return WarmupTrafficPacingSnapshot{Status: status, Reason: reason, ObservedAt: now.UTC().Format(time.RFC3339), BlockingReasons: []string{}}
}

// Snapshot never calls load/prepare/commit: even cold or corrupt reads must not
// populate/evict the account cache, poison faults, initialize or repair a ledger.
func (p *WarmupPacer) Snapshot(account string) WarmupTrafficPacingSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	view := pacingSnapshotStatus(now, "error", "state_unavailable")
	if now.UnixNano() <= 0 {
		return pacingSnapshotStatus(time.Now(), "error", "invalid_clock")
	}
	if account == "" {
		return view
	}
	key := pacingDigest(account)
	a := p.accounts[key]
	if a == nil {
		if p.store == nil {
			return view
		}
		// FileStore.Load only checks and reads bytes; unlike its constructor it
		// cannot create a directory. Keep the decoded account local to this read.
		data, err := p.store.Load(key)
		if errors.Is(err, fs.ErrNotExist) {
			return pacingSnapshotStatus(now, "uninitialized", "ledger_missing")
		}
		if err != nil {
			return view
		}
		a = &pacingAccount{}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if len(data) > warmupPacingMaxBytes || decoder.Decode(&a.state) != nil || decoder.Decode(new(any)) != io.EOF {
			return pacingSnapshotStatus(now, "error", "invalid_state")
		}
	} else if a.fault != nil {
		return view
	} else if a.state.Last == 0 {
		return pacingSnapshotStatus(now, "uninitialized", "ledger_missing")
	}
	if validatePacingState(a.state, key) != nil {
		return pacingSnapshotStatus(now, "error", "invalid_state")
	}
	s := clonePacingState(a.state)
	l := s.Limits
	view.RequestCapacity = &l.Config.RequestBurst
	view.MinAdmissionRequests = &l.Config.MinAdmissionRequests
	view.DailyRequestBudget = &l.DailyRequests
	view.RPMLimit = &l.RPM
	view.MaxActiveBindingGroups = &l.Config.MaxActiveBindings
	view.ActiveBindingIdleSeconds = &l.Config.ActiveBindingIdleSeconds
	view.ConcurrencyLimit = &l.Concurrency
	if !l.Config.Enabled {
		view.Status, view.Reason = "disabled", "policy_disabled"
		return view
	}
	p.advance(a, &s, now.UnixNano())
	// Reuse only the ledger's counters. CountOnly skips group/admission gates;
	// its Allowed/Reason must never be presented as readiness for a conversation.
	d := p.decision(a, s, WarmupPacingRequest{CountOnly: true, EstimateKnown: true}, 0)
	view.Status, view.Reason = "active", "observed"
	view.RequestBalance = &s.Balance
	refill := float64(l.DailyRequests) / 24
	view.RefillPerHour = &refill
	eta := math.Ceil(math.Max(0, float64(l.Config.MinAdmissionRequests)-s.Balance) * 3600 / refill)
	// Unsent reservations occupy capacity. If they prevent reaching the
	// threshold, elapsed time alone cannot produce a truthful balance ETA.
	// A clock rollback also postpones refill until the ledger's Last is
	// reached again; do not advertise an ETA that omits that unknown delay.
	if s.Balance+1e-9 >= float64(l.Config.MinAdmissionRequests) || (now.UnixNano() >= s.Last && pacingAvailableCapacity(a, &s, 0) >= float64(l.Config.MinAdmissionRequests)) {
		view.AdmissionBalanceETASeconds = &eta
	}
	view.Rolling24hRequests, view.Rolling60sRequests = &d.DayRequests, &d.MinuteRequests
	view.ActiveBindingGroups, view.Inflight = &d.ActiveGroups, &d.InFlight
	pending := pacingPendingSends(a, 0)
	view.PendingRequests = &pending
	if s.Balance+1e-9 < float64(l.Config.MinAdmissionRequests) {
		view.BlockingReasons = append(view.BlockingReasons, "request_balance")
	}
	if d.DayRequests >= l.DailyRequests {
		view.BlockingReasons = append(view.BlockingReasons, "daily_budget")
	}
	if d.MinuteRequests >= l.RPM {
		view.BlockingReasons = append(view.BlockingReasons, "rpm")
	}
	if d.ActiveGroups >= l.Config.MaxActiveBindings {
		view.BlockingReasons = append(view.BlockingReasons, "active_groups")
	}
	if d.InFlight >= l.Concurrency {
		view.BlockingReasons = append(view.BlockingReasons, "concurrency")
	}
	if l.DailyTokens > 0 {
		unknownHistory := false
		for id, r := range s.Records {
			if a.live[id] != nil || r.At > s.Last-int64(24*time.Hour) {
				unknownHistory = unknownHistory || (!r.EstimateKnown && !r.UsageComplete)
			}
		}
		for _, bucket := range s.Legacy {
			unknownHistory = unknownHistory || bucket.UnknownTokens
		}
		if unknownHistory {
			view.BlockingReasons = append(view.BlockingReasons, "unknown_token_history")
		}
		if d.Tokens >= l.DailyTokens {
			view.BlockingReasons = append(view.BlockingReasons, "token_budget")
		}
	}
	return view
}

// WarmupTrafficPacingSnapshot observes the manager-owned ledger under the same
// lock order as policy changes, without refreshing policy or legacy accounting.
func (m *Manager) WarmupTrafficPacingSnapshot(account string) WarmupTrafficPacingSnapshot {
	now := time.Now()
	if m == nil {
		return pacingSnapshotStatus(now, "uninitialized", "manager_unavailable")
	}
	m.pacingMu.Lock()
	defer m.pacingMu.Unlock()
	m.mu.RLock()
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	s, _ := m.selector.(*AdaptiveSelector)
	a := m.auths[account].Clone()
	m.mu.RUnlock()
	if s != nil {
		now = s.now()
	}
	if !pacingNativeClaude(a) {
		return pacingSnapshotStatus(now, "not_applicable", "account_ineligible")
	}
	if cfg == nil {
		return pacingSnapshotStatus(now, "uninitialized", "manager_unavailable")
	}
	if cfg.Home.Enabled {
		return pacingSnapshotStatus(now, "not_applicable", "home_mode")
	}
	if s == nil {
		return pacingSnapshotStatus(now, "not_applicable", "non_adaptive")
	}
	if !s.adaptiveEligible(a, cfg.AccountScheduling) {
		return pacingSnapshotStatus(now, "not_applicable", "account_ineligible")
	}
	if s.isMature(a, cfg.AccountScheduling, now) {
		return pacingSnapshotStatus(now, "not_applicable", "mature")
	}
	if !cfg.AccountScheduling.WarmupTrafficPacing.Enabled {
		return pacingSnapshotStatus(now, "disabled", "config_disabled")
	}
	if state := m.pacing; state != nil {
		if state.fault != nil || state.pacer == nil {
			return pacingSnapshotStatus(now, "error", "state_unavailable")
		}
		return state.pacer.Snapshot(account)
	}
	// Recovery can observe a sidecar before the manager has initialized its
	// cache. Do not use NewWarmupPacingFileStore: it calls MkdirAll.
	dir, err := util.ResolveAuthDir(cfg.AuthDir)
	if err == nil {
		dir, err = filepath.Abs(dir)
	}
	if err != nil || dir == "" {
		return pacingSnapshotStatus(now, "error", "state_unavailable")
	}
	p := NewWarmupPacer(&WarmupPacingFileStore{dir: dir}, func() time.Time { return now })
	return p.Snapshot(account)
}
