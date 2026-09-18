package auth

import (
	"testing"
	"time"
)

func TestWarmupPacingLegacyCombinedBudget(t *testing.T) {
	p, _, now, l, r := pacingFixture(t)
	l.Config.RequestBurst = 200
	l.RPM = 200
	r.CountOnly = true
	r.EstimatedTokens = 0
	pacingFill(t, p, now, l, r)
	hour := now.Unix() / 3600
	w := WarmupPacingLegacyWindow{Requests: []DailyWindowBucket{{Hour: hour, Count: 100}}}
	if _, err := p.ReconcileLegacy("account", w); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < min(100, int(8*time.Hour/(432*time.Second))); i++ {
		pacingSendFinish(t, p, l, r, 0)
	}
	// Refill sufficient credits without expiring the imported 100 requests.
	*now = now.Add(8 * time.Hour)
	for i := 0; i < 34; i++ {
		pacingSendFinish(t, p, l, r, 0)
	}
	d, err := p.Peek("account", l, r)
	if err != nil || d.Reason != "daily-budget" || d.DayRequests != 200 {
		t.Fatalf("combined budget: %+v %v", d, err)
	}
	if _, err = p.ReconcileLegacy("account", w); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Peek("account", l, r)
	if d.DayRequests != 200 {
		t.Fatal("reimport double charged", d)
	}
}

func TestWarmupPacingLegacyOwnedAndForeignEvents(t *testing.T) {
	p, _, now, l, r := pacingFixture(t)
	pacingFill(t, p, now, l, r)
	lease := pacingReserve(t, p, l, r)
	if err := p.MarkSent(lease); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(lease, true, 70); err != nil {
		t.Fatal(err)
	}
	hour := now.Unix() / 3600
	w := WarmupPacingLegacyWindow{Requests: []DailyWindowBucket{{Hour: hour, Count: 1}}, Tokens: []DailyWindowBucket{{Hour: hour, Count: 70}}}
	if _, err := p.RecordLegacy("account", w, WarmupPacingLegacyEvent{Requests: 1, Tokens: 70, UnknownTokens: true, Owner: lease}); err != nil {
		t.Fatal(err)
	}
	d, _ := p.Peek("account", l, r)
	if d.DayRequests != 1 || d.Tokens != 70 {
		t.Fatal("owned result charged twice", d)
	}
	if _, err := p.ReconcileLegacy("account", w); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Peek("account", l, r)
	if d.DayRequests != 1 || d.Tokens != 70 {
		t.Fatal("owned watermark lost", d)
	}
	foreign := &WarmupPacingAttempt{pacer: NewWarmupPacer(nil, nil), key: lease.key, sent: true}
	if _, err := p.RecordLegacy("account", w, WarmupPacingLegacyEvent{Requests: 1, Owner: foreign}); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Peek("account", l, r)
	if d.DayRequests != 2 {
		t.Fatal("foreign permit waived charge", d)
	}
	unsent := pacingReserve(t, p, l, r)
	if _, err := p.RecordLegacy("account", w, WarmupPacingLegacyEvent{Requests: 1, Owner: unsent}); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Peek("account", l, r)
	if d.DayRequests != 4 {
		t.Fatal("unsent permit waived charge", d)
	}
}

func TestWarmupPacingLegacyRecoveryFloorAndFailure(t *testing.T) {
	p, store, now, l, r := pacingFixture(t)
	pacingFill(t, p, now, l, r)
	hour := now.Unix() / 3600
	w := WarmupPacingLegacyWindow{Requests: []DailyWindowBucket{{Hour: hour, Count: 100}}}
	if _, err := p.ReconcileLegacy("account", w); err != nil {
		t.Fatal(err)
	}
	restarted := NewWarmupPacer(store, func() time.Time { return *now })
	floor, err := restarted.ReconcileLegacy("account", WarmupPacingLegacyWindow{Requests: []DailyWindowBucket{{Hour: hour, Count: 90}}})
	if err != nil {
		t.Fatal(err)
	}
	g := NewAccountConcurrencyGate(WithGateClock(func() time.Time { return *now }))
	g.RestorePacingLegacyFloor("account", floor)
	updated := g.RecordRequestWindow("account", nil)
	if len(updated) != 1 || updated[0].Count != 101 {
		t.Fatal("stale gate failed to recover floor", updated)
	}
	store.fail = true
	if _, err = restarted.RecordLegacy("account", WarmupPacingLegacyWindow{Requests: updated}, WarmupPacingLegacyEvent{Requests: 1}); err == nil {
		t.Fatal("failed mirror accepted")
	}
	if _, err = restarted.PeekConfigured("account", r); err == nil {
		t.Fatal("poisoned ledger authorized")
	}
	store.fail = false
	restarted = NewWarmupPacer(store, func() time.Time { return *now })
	if _, err = restarted.ReconcileLegacy("account", WarmupPacingLegacyWindow{Requests: updated}); err != nil {
		t.Fatal(err)
	}
	d, _ := restarted.Peek("account", l, r)
	if d.DayRequests != 101 {
		t.Fatal("known increment lost after failed mirror", d)
	}
}

func TestWarmupPacingLegacyUnknownAndConservativeExpiry(t *testing.T) {
	p, store, now, l, r := pacingFixture(t)
	pacingFill(t, p, now, l, r)
	hour := now.Unix() / 3600
	*now = now.Add(59 * time.Minute)
	w := WarmupPacingLegacyWindow{Tokens: []DailyWindowBucket{{Hour: hour, Count: 500}}}
	if _, err := p.ReconcileLegacy("account", w); err != nil {
		t.Fatal(err)
	}
	d, _ := p.Peek("account", l, r)
	if !d.Allowed || d.Tokens != 500 {
		t.Fatal("zero token budget blocked", d)
	}
	l.DailyTokens = 1000
	d, _ = p.Peek("account", l, r)
	if d.Reason != "unknown-token-history" {
		t.Fatal("legacy partial usage treated complete", d)
	}
	*now = time.Unix((hour+24)*3600, 0)
	p = NewWarmupPacer(store, func() time.Time { return *now })
	d, _ = p.Peek("account", l, r)
	if d.Reason != "unknown-token-history" {
		t.Fatal("hour aggregate expired too early", d)
	}
	*now = time.Unix((hour+25)*3600, 0)
	d, _ = p.Peek("account", l, r)
	if !d.Allowed || d.Tokens != 0 {
		t.Fatal("hour debt did not expire", d)
	}
	// A late stream with no usable usage creates a NEW unknown window.
	if _, err := p.RecordLegacy("account", WarmupPacingLegacyWindow{}, WarmupPacingLegacyEvent{UnknownTokens: true}); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Peek("account", l, r)
	if d.Reason != "unknown-token-history" {
		t.Fatal("late unknown usage disappeared", d)
	}
}
