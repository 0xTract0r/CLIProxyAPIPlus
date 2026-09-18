package auth

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
)

// Manager owns the ledger across selector swaps. pacingMu serializes policy,
// legacy counter mutations and sends; no callback takes m.mu from the pacer.
type managerWarmupPacing struct {
	pacer   *WarmupPacer
	dir     string
	fault   error
	managed map[string]bool
}

type pacingOwnershipKey struct{}
type pacingOwnership struct {
	mu      sync.Mutex
	latest  *WarmupPacingAttempt
	manager *Manager
	authID  string
	slot    *accountExecutionSlot
	gated   bool
	started bool
	stream  bool
}

func pacingOwner(ctx context.Context) *WarmupPacingAttempt {
	if ctx == nil {
		return nil
	}
	holder, _ := ctx.Value(pacingOwnershipKey{}).(*pacingOwnership)
	if holder == nil {
		return nil
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	return holder.latest
}

func pacingNativeClaude(a *Auth) bool {
	return a != nil && strings.EqualFold(a.Provider, "claude") && a.Attributes["compat_name"] == ""
}

func pacingLimitsFor(s *AdaptiveSelector, a *Auth, cfg internalconfig.AccountSchedulingConfig, now time.Time) WarmupPacingLimits {
	status := AccountWarmupStatusFor(a, now, cfg)
	scale := AccountRateScale(a, cfg)
	rpm, _ := s.rateLimitParams(a, cfg, now)
	l := WarmupPacingLimits{Config: cfg.WarmupTrafficPacing, DailyRequests: scaleLimitInt(status.DailyBudget, scale), Concurrency: scaleLimitInt(status.ConcurrencyLimit, scale), DailyTokens: int64(scaleLimitInt(resolveTokenDailyBudget(cfg, status), scale))}
	if rpm > 0 {
		l.RPM = max(1, int(math.Floor(rpm)))
	}
	l.Config.Enabled = l.Config.Enabled && !status.Mature && s.adaptiveEligible(a, cfg)
	return l
}

func pacingWindowFromAuth(a *Auth) WarmupPacingLegacyWindow {
	return WarmupPacingLegacyWindow{Requests: readDailyWindowBuckets(a.Metadata, accountSchedulingDailyWindowKey), Tokens: readDailyWindowBuckets(a.Metadata, accountSchedulingTokenWindowKey)}
}

func (m *Manager) refreshWarmupPacing() {
	m.pacingMu.Lock()
	defer m.pacingMu.Unlock()
	m.refreshWarmupPacingLocked()
}

func (m *Manager) refreshWarmupPacingLocked() {
	m.mu.RLock()
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	s, _ := m.selector.(*AdaptiveSelector)
	auths := make([]*Auth, 0, len(m.auths))
	for _, a := range m.auths {
		if pacingNativeClaude(a) {
			auths = append(auths, a.Clone())
		}
	}
	m.mu.RUnlock()
	if cfg == nil || s == nil {
		return
	}
	dir, err := util.ResolveAuthDir(cfg.AuthDir)
	if err == nil {
		dir, err = filepath.Abs(dir)
	}
	enabled := cfg.AccountScheduling.WarmupTrafficPacing.Enabled && !cfg.Home.Enabled
	if m.pacing == nil {
		exists := false
		if err == nil {
			for _, a := range auths {
				if _, statErr := os.Lstat(filepath.Join(dir, pacingDigest(a.ID)+".pacing")); statErr == nil {
					exists = true
					break
				}
			}
		}
		if !enabled && !exists {
			return
		}
		m.pacing = &managerWarmupPacing{dir: dir, managed: make(map[string]bool), fault: err}
		if err == nil {
			store, storeErr := NewWarmupPacingFileStore(dir)
			m.pacing.fault = storeErr
			if storeErr == nil {
				m.pacing.pacer = NewWarmupPacer(store, s.now)
			}
		}
	}
	state := m.pacing
	if s.cache != nil {
		s.cache.mu.Lock()
		s.cache.onServingExit = s.releasePacingBinding
		s.cache.mu.Unlock()
	}
	s.pacer.Store(state.pacer)
	if err != nil || dir != state.dir {
		state.fault = errors.New("warmup pacing state directory changed or unavailable")
	}
	if state.fault != nil {
		log.Warn("warmup pacing unavailable: state directory failure")
		return
	}
	for _, a := range auths {
		limits := pacingLimitsFor(s, a, cfg.AccountScheduling, s.now())
		limits.Config.Enabled = limits.Config.Enabled && enabled
		wasManaged := state.managed[a.ID]
		_, historyErr := os.Lstat(filepath.Join(dir, pacingDigest(a.ID)+".pacing"))
		if !wasManaged && !limits.Config.Enabled {
			if _, err := os.Lstat(filepath.Join(dir, pacingDigest(a.ID)+".pacing")); err != nil {
				continue
			}
		}
		// Disabled recovery loads durable policy before applying its off flag.
		if !limits.Config.Enabled && !wasManaged {
			if _, err := state.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true}); err != nil {
				state.managed[a.ID] = true
				continue
			}
		}
		state.managed[a.ID] = true
		if err := state.pacer.Reconfigure(a.ID, limits); err != nil {
			log.WithField("auth_id", a.ID).Warn("warmup pacing policy unavailable")
			continue
		}
		window := s.gate.PacingLegacyWindow(a.ID, pacingWindowFromAuth(a))
		floor, err := state.pacer.ReconcileLegacy(a.ID, window)
		if err != nil {
			log.WithField("auth_id", a.ID).Warn("warmup pacing legacy reconciliation failed")
			continue
		}
		s.gate.RestorePacingLegacyFloor(a.ID, floor)
		if !wasManaged && os.IsNotExist(historyErr) && a.Success+a.Failed > 0 && len(window.Requests) == 0 && len(window.Tokens) == 0 {
			// Prior reporting may have been disabled. Lifetime activity is not a
			// token measurement and cannot establish an empty recent window.
			if _, err := state.pacer.RecordLegacy(a.ID, window, WarmupPacingLegacyEvent{UnknownTokens: true}); err != nil {
				log.WithField("auth_id", a.ID).Warn("warmup pacing unknown legacy usage persistence failed")
			}
		}
	}
}

type pacingHistoryTransaction struct {
	pacer  *WarmupPacer
	gate   *AccountConcurrencyGate
	authID string
	before WarmupPacingLegacyWindow
}

// The transaction spans old in-memory counter mutation and sidecar commit, but
// never old auth persistence, hooks or cooldown persistence (which lock config).
func (m *Manager) beginPacingHistory(authID string, slots ...*accountExecutionSlot) *pacingHistoryTransaction {
	m.pacingMu.Lock()
	m.mu.RLock()
	a := m.auths[authID]
	g := m.accountConcurrencyGateLocked()
	if len(slots) > 0 && slots[0] != nil && slots[0].gate != nil {
		g = slots[0].gate
	}
	var window WarmupPacingLegacyWindow
	if a != nil && g != nil {
		window = g.PacingLegacyWindow(authID, pacingWindowFromAuth(a))
	}
	m.mu.RUnlock()
	var pacer *WarmupPacer
	if m.pacing != nil && m.pacing.managed[authID] {
		pacer = m.pacing.pacer
	}
	return &pacingHistoryTransaction{pacer, g, authID, window}
}

func pacingWindowIncrement(before, after []DailyWindowBucket) int64 {
	var n int64
	for _, b := range after {
		old := 0
		for _, a := range before {
			if a.Hour == b.Hour {
				old = a.Count
				break
			}
		}
		n = pacingAdd(n, int64(max(0, b.Count-old)))
	}
	return n
}

func (m *Manager) finishPacingHistory(ctx context.Context, txn *pacingHistoryTransaction) {
	if txn == nil {
		return
	}
	defer m.pacingMu.Unlock()
	if txn.pacer == nil || txn.gate == nil {
		return
	}
	window := txn.gate.PacingLegacyWindow(txn.authID, WarmupPacingLegacyWindow{})
	tokens := pacingWindowIncrement(txn.before.Tokens, window.Tokens)
	event := WarmupPacingLegacyEvent{Requests: pacingWindowIncrement(txn.before.Requests, window.Requests), Tokens: tokens, UnknownTokens: tokens > 0, Owner: pacingOwner(ctx), CountOnly: warmupCountPurpose(ctx)}
	floor, err := txn.pacer.RecordLegacy(txn.authID, window, event)
	if err != nil {
		log.WithField("auth_id", txn.authID).Warn("warmup pacing legacy result persistence failed")
		return
	}
	m.mu.RLock()
	currentGate := m.accountConcurrencyGateLocked()
	m.mu.RUnlock()
	if currentGate != nil && currentGate != txn.gate {
		currentGate.RestorePacingLegacyFloor(txn.authID, floor)
	}
	// Health demotion is a policy event. Settle the old refill rate now, not
	// after the next request's idle interval has already accrued at that rate.
	m.refreshWarmupPacingLocked()
}

func (m *Manager) persistCurrentPacingAuth(ctx context.Context, id string) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Preserve the old save ordering and save the current authoritative object,
	// never the snapshot that predates a concurrent refresh/first-use update.
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.auths[id]; a != nil {
		_ = m.persist(context.WithoutCancel(ctx), a)
	}
}

var pacingSafetySink atomic.Pointer[func(context.Context, string, string, int64) bool]

func RegisterAccountPacingUsageSink(fn func(context.Context, string, string, int64) bool) {
	if fn == nil {
		pacingSafetySink.Store(nil)
	} else {
		pacingSafetySink.Store(&fn)
	}
}

// RecordAccountPacingUsage is independent of reporting enablement. Legacy
// records provide a lower bound only; they carry no trustworthy completeness.
func RecordAccountPacingUsage(ctx context.Context, provider, authID string, tokens int64) bool {
	if sink := pacingSafetySink.Load(); sink != nil {
		return (*sink)(ctx, provider, authID, max(0, tokens))
	}
	return false
}

func (m *Manager) recordPacingUsage(ctx context.Context, provider, authID string, tokens int64) bool {
	if !strings.EqualFold(provider, "claude") {
		return false
	}
	txn := m.beginPacingHistory(authID)
	if txn.pacer == nil || txn.gate == nil {
		m.pacingMu.Unlock()
		return false
	}
	m.mu.Lock()
	a := m.auths[authID]
	if a != nil {
		updated := txn.gate.RecordTokensWindow(authID, int(min(tokens, math.MaxInt)), readDailyWindowBuckets(a.Metadata, accountSchedulingTokenWindowKey))
		if a.Metadata == nil {
			a.Metadata = make(map[string]any)
		}
		setAccountSchedulingValue(a.Metadata, accountSchedulingTokenWindowKey, dailyWindowToMetadata(updated))
	}
	m.mu.Unlock()
	window := txn.gate.PacingLegacyWindow(authID, WarmupPacingLegacyWindow{})
	_, err := txn.pacer.RecordLegacy(authID, window, WarmupPacingLegacyEvent{Tokens: tokens, UnknownTokens: true, Owner: pacingOwner(ctx)})
	if err != nil {
		log.WithField("auth_id", authID).Warn("warmup pacing legacy usage persistence failed")
	}
	m.pacingMu.Unlock()
	m.persistCurrentPacingAuth(ctx, authID)
	return true
}
