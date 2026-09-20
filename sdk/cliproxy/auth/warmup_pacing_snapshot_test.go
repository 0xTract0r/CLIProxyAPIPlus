package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWarmupPacingSnapshotReadOnlyRefill(t *testing.T) {
	p, store, now, l, r := pacingFixture(t)
	if got := p.Snapshot("account"); got.Status != "uninitialized" || got.RequestBalance != nil || len(p.accounts) != 0 || store.saves != 0 {
		t.Fatalf("missing ledger initialized: %+v", got)
	}
	if err := p.Reconfigure("account", l); err != nil {
		t.Fatal(err)
	}
	key := pacingDigest("account")
	before := clonePacingState(p.accounts[key].state)
	saved, saves := bytes.Clone(store.data[key]), store.saves
	*now = now.Add(432 * time.Second)
	for range 3 {
		got := p.Snapshot("account")
		if got.Status != "active" || *got.RequestBalance != 1 || *got.RequestCapacity != 8 || *got.MinAdmissionRequests != 4 || *got.AdmissionBalanceETASeconds != 1296 || math.Abs(*got.RefillPerHour-200.0/24) > 1e-9 {
			t.Fatalf("unexpected projection: %+v", got)
		}
		if !reflect.DeepEqual(got.BlockingReasons, []string{"request_balance"}) || *got.PendingRequests != 0 {
			t.Fatalf("snapshot claims admission from a one-credit count request: %+v", got)
		}
	}
	if !reflect.DeepEqual(before, p.accounts[key].state) || !bytes.Equal(saved, store.data[key]) || store.saves != saves {
		t.Fatal("snapshot advanced memory or durable state")
	}
	*now = now.Add(-time.Hour)
	if got := p.Snapshot("account"); *got.RequestBalance != 0 || got.AdmissionBalanceETASeconds != nil || got.ObservedAt != now.UTC().Format(time.RFC3339) {
		t.Fatal("clock rollback granted credit or a misleading ETA", got)
	}
	if !reflect.DeepEqual(before, p.accounts[key].state) || !bytes.Equal(saved, store.data[key]) || store.saves != saves {
		t.Fatal("rollback snapshot changed balance, time anchor, or durable state")
	}
	_ = r
}

func TestWarmupPacingSnapshotRollbackWithAdmissionBalance(t *testing.T) {
	p, store, now, l, r := pacingFixture(t)
	pacingFill(t, p, now, l, r)
	attempt := pacingReserve(t, p, l, r)
	if err := p.CancelUnsent(attempt); err != nil {
		t.Fatal(err)
	}
	key := pacingDigest("account")
	before := clonePacingState(p.accounts[key].state)
	saved, saves := bytes.Clone(store.data[key]), store.saves
	*now = now.Add(-time.Hour)
	got := p.Snapshot("account")
	if *got.RequestBalance != 8 || got.AdmissionBalanceETASeconds == nil || *got.AdmissionBalanceETASeconds != 0 {
		t.Fatal("sufficient existing balance must retain a zero ETA", got)
	}
	if !reflect.DeepEqual(before, p.accounts[key].state) || !bytes.Equal(saved, store.data[key]) || store.saves != saves {
		t.Fatal("rollback snapshot changed the ledger")
	}
}

func TestWarmupPacingSnapshotPendingAndExpiry(t *testing.T) {
	p, store, now, l, r := pacingFixture(t)
	pacingFill(t, p, now, l, r)
	attempt := pacingReserve(t, p, l, r)
	key := pacingDigest("account")
	before := clonePacingState(p.accounts[key].state)
	saves := store.saves
	*now = now.Add(25 * time.Hour)
	got := p.Snapshot("account")
	if *got.Rolling24hRequests != 1 || *got.Rolling60sRequests != 1 || *got.PendingRequests != 1 || *got.Inflight != 1 || *got.ActiveBindingGroups != 1 || *got.RequestBalance != 7 {
		t.Fatalf("unsent reservation expired or refilled: %+v", got)
	}
	if !slices.Contains(got.BlockingReasons, "active_groups") || !slices.Contains(got.BlockingReasons, "concurrency") {
		t.Fatal("missing independent resource blockers", got)
	}
	if !reflect.DeepEqual(before, p.accounts[key].state) || store.saves != saves || attempt.sent || attempt.done {
		t.Fatal("read mutated reservation")
	}
	if err := p.MarkSent(attempt); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(attempt, true, 10); err != nil {
		t.Fatal(err)
	}
	before = clonePacingState(p.accounts[key].state)
	saves = store.saves
	*now = now.Add(25 * time.Hour)
	got = p.Snapshot("account")
	if *got.Rolling24hRequests != 0 || *got.Rolling60sRequests != 0 || *got.ActiveBindingGroups != 0 || *got.Inflight != 0 || *got.PendingRequests != 0 || *got.RequestBalance != 8 {
		t.Fatalf("expired history not projected: %+v", got)
	}
	if !reflect.DeepEqual(before, p.accounts[key].state) || store.saves != saves {
		t.Fatal("window cleanup changed authoritative state")
	}
}

func TestWarmupPacingSnapshotColdAndCorruptReads(t *testing.T) {
	p, store, now, l, _ := pacingFixture(t)
	if err := p.Reconfigure("account", l); err != nil {
		t.Fatal(err)
	}
	key := pacingDigest("account")
	saved := bytes.Clone(store.data[key])
	saves := store.saves
	*now = now.Add(time.Hour)
	cold := NewWarmupPacer(store, func() time.Time { return *now })
	got := cold.Snapshot("account")
	if got.Status != "active" || *got.RequestBalance != 8 || *got.DailyRequestBudget != 200 || len(cold.accounts) != 0 || store.saves != saves {
		t.Fatalf("cold read failed or populated cache: %+v", got)
	}
	store.data[key] = []byte("/private/secret-account: bad ledger token-secret")
	got = cold.Snapshot("account")
	encoded, _ := json.Marshal(got)
	if got.Status != "error" || got.Reason != "invalid_state" || got.RequestBalance != nil || got.RequestCapacity != nil || len(cold.accounts) != 0 || strings.Contains(string(encoded), "secret") {
		t.Fatalf("corruption leaked or populated fault cache: %s", encoded)
	}
	store.data[key] = saved
	if got = cold.Snapshot("account"); got.Status != "active" {
		t.Fatal("failed read poisoned later observation", got)
	}
	p.accounts[key].fault = errors.New("/private/secret-account token-secret")
	got = p.Snapshot("account")
	if got.Status != "error" || got.Reason != "state_unavailable" || got.RequestBalance != nil || p.accounts[key].fault == nil {
		t.Fatal("cached storage fault was exposed or cleared", got)
	}
}

func TestWarmupPacingSnapshotBlocksAndReservationETA(t *testing.T) {
	p, _, now, l, r := pacingFixture(t)
	l.Config.MinAdmissionRequests = 8
	l.DailyTokens = 100
	pacingFill(t, p, now, l, r)
	attempt := pacingReserve(t, p, l, r)
	got := p.Snapshot("account")
	if got.AdmissionBalanceETASeconds != nil || !slices.Contains(got.BlockingReasons, "request_balance") || !slices.Contains(got.BlockingReasons, "token_budget") {
		t.Fatal("reservation-constrained balance advertised a time-only ETA", got)
	}
	a := p.accounts[pacingDigest("account")]
	s := clonePacingState(a.state)
	s.Limits.DailyRequests, s.Limits.RPM = 1, 1
	raw := s.Records[attempt.id]
	raw.EstimateKnown = false
	s.Records[attempt.id] = raw
	if err := p.commit(a, s); err != nil {
		t.Fatal(err)
	}
	got = p.Snapshot("account")
	want := []string{"request_balance", "daily_budget", "rpm", "active_groups", "concurrency", "unknown_token_history", "token_budget"}
	if !reflect.DeepEqual(got.BlockingReasons, want) {
		t.Fatalf("blockers = %v, want %v", got.BlockingReasons, want)
	}
	off := s.Limits
	off.Config.Enabled = false
	if err := p.Reconfigure("account", off); err != nil {
		t.Fatal(err)
	}
	got = p.Snapshot("account")
	if got.Status != "disabled" || got.RequestBalance != nil || got.Rolling24hRequests != nil || got.RefillPerHour != nil || *got.RequestCapacity != 8 || len(got.BlockingReasons) != 0 {
		t.Fatal("disabled ledger emitted dynamic values or lost reliable limits", got)
	}
}

func TestWarmupPacingSnapshotConcurrentReadAndReserve(t *testing.T) {
	p, _, now, l, r := pacingFixture(t)
	pacingFill(t, p, now, l, r)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				got := p.Snapshot("account")
				if got.Status != "active" || *got.RequestBalance < 0 || *got.PendingRequests > 1 {
					t.Errorf("inconsistent concurrent read: %+v", got)
				}
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			a, _, err := p.Reserve("account", l, r)
			if err != nil || a == nil {
				t.Errorf("reserve failed: %v", err)
				return
			}
			if err = p.CancelUnsent(a); err != nil {
				t.Error(err)
				return
			}
		}
	})
	wg.Wait()
	if got := p.Snapshot("account"); *got.RequestBalance != 8 || *got.Rolling24hRequests != 0 {
		t.Fatal("reads consumed or minted credits", got)
	}
}

func TestWarmupPacingManagerSnapshotReadOnly(t *testing.T) {
	m, _, a, cfg, now := pacingManagerFixture(t)
	path := filepath.Join(cfg.AuthDir, pacingDigest(a.ID)+".pacing")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	*now = now.Add(432 * time.Second)
	old := m.pacing
	beforeState := clonePacingState(old.pacer.accounts[pacingDigest(a.ID)].state)
	got := m.WarmupTrafficPacingSnapshot(a.ID)
	if got.Status != "active" || *got.RequestBalance != 1 {
		t.Fatal("manager did not project refill", got)
	}
	if !reflect.DeepEqual(beforeState, old.pacer.accounts[pacingDigest(a.ID)].state) {
		t.Fatal("manager refreshed authoritative ledger")
	}
	// Config publication without reconciliation must not invent new limits.
	changed := *cfg
	changed.AccountScheduling.WarmupTrafficPacing.RequestBurst = 99
	m.runtimeConfig.Store(&changed)
	if got = m.WarmupTrafficPacingSnapshot(a.ID); *got.RequestCapacity != 8 {
		t.Fatal("snapshot substituted configured limits for ledger policy", got)
	}
	m.pacing = nil
	got = m.WarmupTrafficPacingSnapshot(a.ID)
	if got.Status != "active" || *got.RequestCapacity != 8 || m.pacing != nil {
		t.Fatal("cold manager created a pacer or ignored durable policy", got)
	}
	after, _ := os.ReadFile(path)
	afterInfo, _ := os.Stat(path)
	if !bytes.Equal(before, after) || !info.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("GET-style reads rewrote sidecar")
	}
	changed.AuthDir = filepath.Join(t.TempDir(), "not-created")
	got = m.WarmupTrafficPacingSnapshot(a.ID)
	if got.Status != "uninitialized" || got.RequestBalance != nil || m.pacing != nil {
		t.Fatal("missing ledger was initialized", got)
	}
	if _, err := os.Stat(changed.AuthDir); !os.IsNotExist(err) {
		t.Fatal("read created a storage directory", err)
	}
	m.pacing = old
}

func TestWarmupPacingManagerSnapshotStates(t *testing.T) {
	m, s, a, cfg, _ := pacingManagerFixture(t)
	tests := []struct {
		name, status, reason string
		change               func()
	}{
		{"disabled", "disabled", "config_disabled", func() { c := *cfg; c.AccountScheduling.WarmupTrafficPacing.Enabled = false; m.runtimeConfig.Store(&c) }},
		{"home", "not_applicable", "home_mode", func() { c := *cfg; c.Home.Enabled = true; m.runtimeConfig.Store(&c) }},
		{"non-adaptive", "not_applicable", "non_adaptive", func() { m.selector = &RoundRobinSelector{} }},
		{"mature", "not_applicable", "mature", func() { c := *cfg; c.AccountScheduling.WarmupCurve = nil; m.runtimeConfig.Store(&c) }},
		{"state-fault", "error", "state_unavailable", func() { m.pacing.fault = errors.New("private path secret") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m.runtimeConfig.Store(cfg)
			m.selector = s
			m.pacing.fault = nil
			tt.change()
			got := m.WarmupTrafficPacingSnapshot(a.ID)
			if got.Status != tt.status || got.Reason != tt.reason || got.RequestBalance != nil || got.PendingRequests != nil || len(got.BlockingReasons) != 0 {
				t.Fatalf("unexpected inactive state: %+v", got)
			}
		})
	}
}

func TestWarmupPacingManagerSnapshotConcurrentConfig(t *testing.T) {
	m, _, a, cfg, _ := pacingManagerFixture(t)
	off := *cfg
	off.AccountScheduling.WarmupTrafficPacing.Enabled = false
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 10 {
			m.SetConfig(&off)
			m.SetConfig(cfg)
		}
	})
	for range 4 {
		wg.Go(func() {
			for range 50 {
				got := m.WarmupTrafficPacingSnapshot(a.ID)
				if got.Status != "active" && got.Status != "disabled" {
					t.Errorf("inconsistent config observation: %+v", got)
				}
			}
		})
	}
	wg.Wait()
}
