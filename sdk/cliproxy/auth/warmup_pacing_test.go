package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type pacingMemoryStore struct {
	data            map[string][]byte
	fail, ambiguous bool
	saves           int
}

func (s *pacingMemoryStore) Load(key string) ([]byte, error) {
	if b, ok := s.data[key]; ok {
		return bytes.Clone(b), nil
	}
	return nil, fs.ErrNotExist
}
func (s *pacingMemoryStore) Save(key string, b []byte) error {
	s.saves++
	if s.fail && !s.ambiguous {
		return errors.New("synthetic storage failure")
	}
	if s.data == nil {
		s.data = make(map[string][]byte)
	}
	s.data[key] = bytes.Clone(b)
	if s.fail {
		return errors.New("synthetic post-rename fsync failure")
	}
	return nil
}

func pacingFixture(t *testing.T) (*WarmupPacer, *pacingMemoryStore, *time.Time, WarmupPacingLimits, WarmupPacingRequest) {
	t.Helper()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	store := &pacingMemoryStore{}
	p := NewWarmupPacer(store, func() time.Time { return now })
	cfg := internalconfig.DefaultWarmupTrafficPacingConfig()
	cfg.Enabled = true
	limits := WarmupPacingLimits{Config: cfg, DailyRequests: 200, RPM: 3, Concurrency: 1}
	req := WarmupPacingRequest{GroupID: "synthetic-group", BindingID: "synthetic-parent", EstimateKnown: true, EstimatedTokens: 100}
	return p, store, &now, limits, req
}

func pacingFill(t *testing.T, p *WarmupPacer, now *time.Time, l WarmupPacingLimits, r WarmupPacingRequest) {
	t.Helper()
	d, err := p.Peek("account", l, r)
	if err != nil || d.Allowed || d.Balance != 0 {
		t.Fatalf("initial balance was not zero: %+v %v", d, err)
	}
	*now = now.Add(8 * time.Hour)
}

func pacingReserve(t *testing.T, p *WarmupPacer, l WarmupPacingLimits, r WarmupPacingRequest) *WarmupPacingAttempt {
	t.Helper()
	a, d, err := p.Reserve("account", l, r)
	if err != nil || !d.Allowed || a == nil {
		t.Fatalf("reserve: %+v %v", d, err)
	}
	return a
}

func pacingSendFinish(t *testing.T, p *WarmupPacer, l WarmupPacingLimits, r WarmupPacingRequest, tokens int64) {
	t.Helper()
	a := pacingReserve(t, p, l, r)
	if err := p.MarkSent(a); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(a, true, tokens); err != nil {
		t.Fatal(err)
	}
}

func TestWarmupPacingRefillAndRateChanges(t *testing.T) {
	p, store, now, l, r := pacingFixture(t)
	d, err := p.Peek("account", l, r)
	if err != nil || d.Allowed || d.Balance != 0 || store.saves != 1 {
		t.Fatalf("initial anchor: %+v %v", d, err)
	}
	*now = now.Add(432 * time.Second)
	d, _ = p.Peek("account", l, r)
	if math.Abs(d.Balance-1) > 1e-9 || d.Allowed {
		t.Fatalf("200/day refill: %+v", d)
	}
	l.DailyRequests = 400
	if err := p.Reconfigure("account", l); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(432 * time.Second)
	d, _ = p.Peek("account", l, r)
	if math.Abs(d.Balance-3) > 1e-9 {
		t.Fatalf("rate change was retroactive: %+v", d)
	}
	*now = now.Add(8 * time.Hour)
	d, _ = p.Peek("account", l, r)
	if d.Balance != 8 || !d.Allowed {
		t.Fatalf("idle cap: %+v", d)
	}
	lease := pacingReserve(t, p, l, r)
	*now = now.Add(-time.Hour)
	d, _ = p.Peek("account", l, r)
	if d.Balance != 7 {
		t.Fatalf("rollback granted balance: %+v", d)
	}
	if err := p.CancelUnsent(lease); err != nil {
		t.Fatal(err)
	}
	l.Config.RequestBurst = 5
	if err := p.Reconfigure("account", l); err != nil {
		t.Fatal(err)
	}
	l.Config.RequestBurst = 9
	if err := p.Reconfigure("account", l); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Peek("account", l, r)
	if d.Balance != 5 {
		t.Fatalf("capacity increase minted balance: %+v", d)
	}
}

func TestWarmupPacingAtomicBurstAndWindows(t *testing.T) {
	p, _, now, l, r := pacingFixture(t)
	pacingFill(t, p, now, l, r)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	leas := make(chan *WarmupPacingAttempt, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, d, err := p.Reserve("account", l, r)
			if err != nil {
				t.Error(err)
			}
			if d.Allowed {
				accepted.Add(1)
				leas <- a
			}
		}()
	}
	wg.Wait()
	close(leas)
	if accepted.Load() != 1 {
		t.Fatalf("concurrency over-admitted %d", accepted.Load())
	}
	for a := range leas {
		if err := p.MarkSent(a); err != nil {
			t.Fatal(err)
		}
		if err := p.Finish(a, false, 0); err != nil {
			t.Fatal(err)
		}
	}
	pacingSendFinish(t, p, l, r, 1)
	pacingSendFinish(t, p, l, r, 1)
	d, _ := p.Peek("account", l, r)
	if d.Allowed || d.Reason != "rpm" || d.MinuteRequests != 3 || d.DayRequests != 3 {
		t.Fatalf("RPM backstop: %+v", d)
	}
	*now = now.Add(time.Minute - time.Nanosecond)
	d, _ = p.Peek("account", l, r)
	if d.Allowed {
		t.Fatal("60s window ended early")
	}
	*now = now.Add(time.Nanosecond)
	d, _ = p.Peek("account", l, r)
	if !d.Allowed || d.MinuteRequests != 0 || d.DayRequests != 3 {
		t.Fatalf("60s exact boundary: %+v", d)
	}
	l.DailyRequests = 3
	if err := p.Reconfigure("account", l); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Peek("account", l, r)
	if d.Reason != "daily-budget" {
		t.Fatalf("budget ignored after limit reduction: %+v", d)
	}
	*now = now.Add(24*time.Hour - time.Minute - time.Nanosecond)
	d, _ = p.Peek("account", l, r)
	if d.DayRequests != 3 {
		t.Fatal("24h window ended early")
	}
	*now = now.Add(time.Nanosecond)
	d, _ = p.Peek("account", l, r)
	if d.DayRequests != 0 {
		t.Fatalf("24h exact boundary: %+v", d)
	}
}

func TestWarmupPacingGroupMembershipAndCount(t *testing.T) {
	p, _, now, l, r := pacingFixture(t)
	pacingFill(t, p, now, l, r)
	pacingSendFinish(t, p, l, r, 1)
	fork := r
	fork.BindingID = "synthetic-fork"
	pacingSendFinish(t, p, l, fork, 1)
	if err := p.ReleaseBinding("account", r.GroupID, fork.BindingID); err != nil {
		t.Fatal(err)
	}
	other := r
	other.GroupID = "another-group"
	other.BindingID = "another-child"
	d, _ := p.Peek("account", l, other)
	if d.Reason != "active-groups" || d.ActiveGroups != 1 {
		t.Fatalf("fork released parent group: %+v", d)
	}
	*now = now.Add(299 * time.Second)
	count := WarmupPacingRequest{CountOnly: true}
	pacingSendFinish(t, p, l, count, 0)
	*now = now.Add(time.Second)
	d, _ = p.Peek("account", l, other)
	if !d.Allowed || d.ActiveGroups != 0 {
		t.Fatalf("Count renewed serving group: %+v", d)
	}
	a := pacingReserve(t, p, l, other)
	if err := p.CancelUnsent(a); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Peek("account", l, r)
	if !d.Allowed || d.ActiveGroups != 0 {
		t.Fatalf("cancel leaked group: %+v", d)
	}
}

func TestWarmupPacingTokenSettlement(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "complete"}[complete], func(t *testing.T) {
			p, _, now, l, r := pacingFixture(t)
			l.DailyTokens = 150
			pacingFill(t, p, now, l, r)
			a := pacingReserve(t, p, l, r)
			d, _ := p.Peek("account", l, r)
			if d.Reason != "token-budget" || d.PendingTokens != 100 || d.Tokens != 100 {
				t.Fatalf("pending tokens absent: %+v", d)
			}
			if err := p.MarkSent(a); err != nil {
				t.Fatal(err)
			}
			if err := p.Finish(a, complete, 20); err != nil {
				t.Fatal(err)
			}
			if err := p.Finish(a, true, 0); err != nil {
				t.Fatal(err)
			}
			d, _ = p.Peek("account", l, r)
			want := int64(100)
			if complete {
				want = 20
			}
			if d.Tokens != want || d.PendingTokens != 0 {
				t.Fatalf("settlement/duplicate changed tokens: %+v", d)
			}
			if !complete && d.Allowed {
				t.Fatal("unknown usage refunded estimate")
			}
		})
	}
	p, _, now, l, r := pacingFixture(t)
	l.DailyTokens = 150
	pacingFill(t, p, now, l, r)
	a := pacingReserve(t, p, l, r)
	if err := p.MarkSent(a); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(a, false, 200); err != nil {
		t.Fatal(err)
	}
	d, _ := p.Peek("account", l, r)
	if d.Tokens != 200 || d.Reason != "token-budget" {
		t.Fatalf("token debt not preserved: %+v", d)
	}
	r.EstimateKnown = false
	d, _ = p.Peek("account", l, r)
	if d.Reason != "unknown-token-estimate" {
		t.Fatalf("unknown estimate accepted: %+v", d)
	}
	l.DailyTokens = 0
	d, _ = p.Peek("account", l, r)
	if !d.Allowed {
		t.Fatalf("zero budget did not disable token gate: %+v", d)
	}
}

func TestWarmupPacingCancelRecoveryAndDisable(t *testing.T) {
	p, store, now, l, r := pacingFixture(t)
	pacingFill(t, p, now, l, r)
	a := pacingReserve(t, p, l, r)
	if err := p.CancelUnsent(a); err != nil {
		t.Fatal(err)
	}
	if err := p.CancelUnsent(a); err != nil {
		t.Fatal(err)
	}
	d, _ := p.Peek("account", l, r)
	if d.Balance != 8 || d.DayRequests != 0 || d.InFlight != 0 {
		t.Fatalf("unsent refund: %+v", d)
	}
	_ = pacingReserve(t, p, l, r)
	restarted := NewWarmupPacer(store, func() time.Time { return *now })
	d, err := restarted.Peek("account", l, r)
	if err != nil || d.Balance != 7 || d.DayRequests != 1 || d.InFlight != 0 || d.ActiveGroups != 0 {
		t.Fatalf("crash refunded or restored stale lease: %+v %v", d, err)
	}
	off := l
	off.Config.Enabled = false
	if err := restarted.Reconfigure("account", off); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	if err := restarted.Reconfigure("account", l); err != nil {
		t.Fatal(err)
	}
	d, _ = restarted.Peek("account", l, r)
	if d.Balance != 8 || d.DayRequests != 1 {
		t.Fatalf("reopen reset history: %+v", d)
	}
	badStore := &pacingMemoryStore{fail: true}
	d, err = NewWarmupPacer(badStore, nil).Peek("account", off, r)
	if err != nil || !d.Allowed || badStore.saves != 0 {
		t.Fatal("disabled pacing touched storage")
	}
}

func TestWarmupPacingSaveFailureClosed(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-write", true: "after-rename"}[ambiguous], func(t *testing.T) {
			p, store, now, l, r := pacingFixture(t)
			pacingFill(t, p, now, l, r)
			old := clonePacingState(p.accounts[pacingDigest("account")].state)
			store.fail, store.ambiguous = true, ambiguous
			a, d, err := p.Reserve("account", l, r)
			if err == nil || a != nil || d.Allowed {
				t.Fatal("failed persistence authorized send")
			}
			current := p.accounts[pacingDigest("account")].state
			if current.Next != old.Next || current.Balance != old.Balance {
				t.Fatal("failed persistence published state")
			}
			store.fail = false
			if _, err := p.Peek("account", l, r); err == nil {
				t.Fatal("ambiguous state silently recovered in process")
			}
		})
	}
}

func TestWarmupPacingSchemaAndFileStore(t *testing.T) {
	p, store, now, l, r := pacingFixture(t)
	pacingFill(t, p, now, l, r)
	key := pacingDigest("account")
	for _, data := range [][]byte{[]byte("broken"), []byte(`{"schema":999}`), append(bytes.Clone(store.data[key]), []byte("{}")...)} {
		broken := &pacingMemoryStore{data: map[string][]byte{key: data}}
		if _, err := NewWarmupPacer(broken, func() time.Time { return *now }).Peek("account", l, r); err == nil {
			t.Fatal("corrupt state accepted")
		}
	}
	dir := t.TempDir()
	files, err := NewWarmupPacingFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Save(key, store.data[key]); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, key+".pacing")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("sidecar mode: %v %v", info, err)
	}
	data, err := files.Load(key)
	if err != nil || !bytes.Equal(data, store.data[key]) {
		t.Fatal("sidecar roundtrip", err)
	}
	if err := files.Save("../outside", nil); err == nil {
		t.Fatal("store accepted path traversal")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Load(key); err == nil {
		t.Fatal("unsafe file mode accepted")
	}
	var saved pacingState
	if err := json.Unmarshal(data, &saved); err != nil || saved.Schema != warmupPacingSchema {
		t.Fatal("sidecar schema", err)
	}
}

func TestWarmupPacingPreSendReconfiguration(t *testing.T) {
	t.Run("unknown-estimate-budget-enabled", func(t *testing.T) {
		p, _, now, l, r := pacingFixture(t)
		r.EstimateKnown = false
		pacingFill(t, p, now, l, r)
		a := pacingReserve(t, p, l, r)
		l.DailyTokens = 1000
		if err := p.Reconfigure("account", l); err != nil {
			t.Fatal(err)
		}
		if err := p.MarkSent(a); err == nil || a.sent {
			t.Fatal("new positive token budget accepted an unknown zero estimate")
		}
		if err := p.CancelUnsent(a); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("active-group-limit-reduced", func(t *testing.T) {
		p, _, now, l, r := pacingFixture(t)
		l.Config.MaxActiveBindings = 2
		l.Concurrency = 2
		pacingFill(t, p, now, l, r)
		a := pacingReserve(t, p, l, r)
		r.GroupID = "other-group"
		r.BindingID = "other-binding"
		b := pacingReserve(t, p, l, r)
		l.Config.MaxActiveBindings = 1
		if err := p.Reconfigure("account", l); err != nil {
			t.Fatal(err)
		}
		if err := p.MarkSent(a); err == nil || a.sent {
			t.Fatal("reduced group limit ignored pending admissions")
		}
		if err := p.CancelUnsent(b); err != nil {
			t.Fatal(err)
		}
		if err := p.MarkSent(a); err != nil {
			t.Fatal("remaining sole group refused", err)
		}
	})
	t.Run("unknown-history-survives-restart", func(t *testing.T) {
		p, store, now, l, r := pacingFixture(t)
		r.EstimateKnown = false
		pacingFill(t, p, now, l, r)
		a := pacingReserve(t, p, l, r)
		if err := p.MarkSent(a); err != nil {
			t.Fatal(err)
		}
		if err := p.Finish(a, false, 0); err != nil {
			t.Fatal(err)
		}
		p = NewWarmupPacer(store, func() time.Time { return *now })
		l.DailyTokens = 1000
		r.EstimateKnown = true
		d, err := p.Peek("account", l, r)
		if err != nil || d.Allowed || d.Reason != "unknown-token-history" {
			t.Fatalf("unknown usage became known zero: %+v %v", d, err)
		}
	})
}

func TestWarmupPacingLongAttemptActivity(t *testing.T) {
	for _, released := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "released"}[released], func(t *testing.T) {
			p, _, now, l, r := pacingFixture(t)
			pacingFill(t, p, now, l, r)
			a := pacingReserve(t, p, l, r)
			if err := p.MarkSent(a); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(301 * time.Second)
			if released {
				if err := p.ReleaseBinding("account", r.GroupID, r.BindingID); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.Finish(a, true, 1); err != nil {
				t.Fatal(err)
			}
			other := r
			other.GroupID = "other-group"
			other.BindingID = "other-binding"
			d, err := p.Peek("account", l, other)
			if err != nil || d.Allowed == !released {
				t.Fatalf("finished activity mismatch: released=%v decision=%+v err=%v", released, d, err)
			}
		})
	}
}

func TestWarmupPacingPendingSendOccupiesBurst(t *testing.T) {
	p, _, now, l, r := pacingFixture(t)
	pacingFill(t, p, now, l, r)
	old := pacingReserve(t, p, l, r)
	*now = now.Add(432 * time.Second)
	if err := p.MarkSent(old); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(old, true, 1); err != nil {
		t.Fatal(err)
	}
	started := *now
	sent := 1
	for i := 1; i < 9; i++ {
		*now = started.Add(time.Duration(i/3) * time.Minute)
		a, d, err := p.Reserve("account", l, r)
		if err != nil {
			t.Fatal(err)
		}
		if !d.Allowed {
			if d.Reason != "request-balance" {
				t.Fatalf("unexpected denial: %+v", d)
			}
			continue
		}
		if err := p.MarkSent(a); err != nil {
			t.Fatal(err)
		}
		if err := p.Finish(a, true, 1); err != nil {
			t.Fatal(err)
		}
		sent++
	}
	bound := 8 + 120.0/432.0
	if float64(sent) > bound || sent != 8 {
		t.Fatalf("pending refill minted a request: sent=%d in 120s, allowance=%f", sent, bound)
	}
}

func TestWarmupPacingPendingBurstReductionRefund(t *testing.T) {
	p, _, now, l, r := pacingFixture(t)
	l.Config.MinAdmissionRequests = 1
	l.Concurrency = 3
	pacingFill(t, p, now, l, r)
	a := pacingReserve(t, p, l, r)
	b := pacingReserve(t, p, l, r)
	c := pacingReserve(t, p, l, r)
	l.Config.RequestBurst = 2
	if err := p.Reconfigure("account", l); err != nil {
		t.Fatal(err)
	}
	d, err := p.Peek("account", l, r)
	if err != nil || d.Balance != 0 {
		t.Fatalf("smaller bucket ignored pending occupancy: %+v %v", d, err)
	}
	if err := p.MarkSent(a); err == nil {
		t.Fatal("sent more reserved credits than the reduced bucket capacity")
	}
	if err := p.CancelUnsent(a); err != nil {
		t.Fatal(err)
	}
	if err := p.CancelUnsent(a); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Peek("account", l, r)
	if d.Balance != 0 {
		t.Fatalf("refund exceeded room behind two pending credits: %+v", d)
	}
	if err := p.CancelUnsent(b); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Peek("account", l, r)
	if d.Balance != 1 {
		t.Fatalf("refund behind one pending credit: %+v", d)
	}
	if err := p.MarkSent(c); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(c, true, 1); err != nil {
		t.Fatal(err)
	}
}

func TestWarmupPacingPendingAdmissionRecheck(t *testing.T) {
	for _, withPeer := range []bool{false, true} {
		t.Run(map[bool]string{false: "own-credit", true: "same-group-peer"}[withPeer], func(t *testing.T) {
			p, _, now, l, r := pacingFixture(t)
			l.Concurrency = 2
			if _, err := p.Peek("account", l, r); err != nil {
				t.Fatal(err)
			}
			balance := 4
			if withPeer {
				balance = 5
			}
			*now = now.Add(time.Duration(balance) * 432 * time.Second)
			a := pacingReserve(t, p, l, r)
			var b *WarmupPacingAttempt
			if withPeer {
				peer := r
				peer.BindingID = "pending-fork"
				b = pacingReserve(t, p, l, peer)
			}
			l.Config.MinAdmissionRequests = 8
			if err := p.Reconfigure("account", l); err != nil {
				t.Fatal(err)
			}
			if err := p.MarkSent(a); err == nil || a.sent {
				t.Fatal("pending group bypassed increased first-admission threshold")
			}
			if b != nil {
				if err := p.MarkSent(b); err == nil || b.sent {
					t.Fatal("peer pending group waived first-admission threshold")
				}
				if err := p.CancelUnsent(b); err != nil {
					t.Fatal(err)
				}
			}
			l.Config.MinAdmissionRequests = 4
			if err := p.Reconfigure("account", l); err != nil {
				t.Fatal(err)
			}
			if err := p.MarkSent(a); err != nil {
				t.Fatal("own prepaid credit was omitted from threshold check", err)
			}
		})
	}
}

func TestWarmupPacingRejectsLeaseReuse(t *testing.T) {
	p, store, now, l, r := pacingFixture(t)
	pacingFill(t, p, now, l, r)
	a := pacingReserve(t, p, l, r)
	if err := p.CancelUnsent(a); err != nil {
		t.Fatal(err)
	}
	before, err := p.Peek("account", l, r)
	if err != nil {
		t.Fatal(err)
	}
	saves := store.saves
	if err := p.MarkSent(a); err == nil {
		t.Fatal("refunded lease authorized sending")
	}
	after, err := p.Peek("account", l, r)
	if err != nil || before != after || saves != store.saves {
		t.Fatalf("refunded lease changed accounting: before=%+v after=%+v err=%v", before, after, err)
	}
	b := pacingReserve(t, p, l, r)
	if err := p.MarkSent(b); err != nil {
		t.Fatal(err)
	}
	before, err = p.Peek("account", l, r)
	if err != nil {
		t.Fatal(err)
	}
	saves = store.saves
	if err := p.MarkSent(b); err == nil {
		t.Fatal("sent lease authorized another attempt")
	}
	if err := p.CancelUnsent(b); err != nil {
		t.Fatal(err)
	}
	after, err = p.Peek("account", l, r)
	if err != nil || before != after || saves != store.saves || after.DayRequests != 1 || after.InFlight != 1 {
		t.Fatalf("sent lease changed accounting: before=%+v after=%+v err=%v", before, after, err)
	}
	if err := p.Finish(b, true, 1); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkSent(b); err == nil {
		t.Fatal("finished lease authorized another attempt")
	}
	if err := p.MarkSent(nil); err != nil {
		t.Fatal("disabled nil lease changed behavior", err)
	}
}
