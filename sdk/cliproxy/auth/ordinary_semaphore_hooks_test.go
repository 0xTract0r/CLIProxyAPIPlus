package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestOrdinarySDKGateRetainsConcurrentAcquireAndRelease(t *testing.T) {
	gate := NewAccountConcurrencyGate()
	const total = 20
	admitted := make(chan bool, total)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for range total {
		wg.Go(func() {
			admitted <- gate.Acquire("fixture", 3)
			<-release
			gate.Release("fixture")
		})
	}
	var withinLimit int
	for range total {
		if <-admitted {
			withinLimit++
		}
	}
	if withinLimit != 3 || gate.InFlight("fixture") != total || gate.InFlight("other") != 0 {
		t.Errorf("admitted=%d in-flight=%d other=%d", withinLimit, gate.InFlight("fixture"), gate.InFlight("other"))
	}
	close(release)
	wg.Wait()
	gate.Release("fixture")
	if gate.InFlight("fixture") != 0 || !gate.Acquire("", 1) {
		t.Fatal("release did not restore an empty gate")
	}
}

type ordinaryCountingHook struct {
	NoopHook
	registered atomic.Int64
}

func (h *ordinaryCountingHook) OnAuthRegistered(context.Context, *Auth) {
	h.registered.Add(1)
}

func TestOrdinarySDKSetHookPreservesConcurrentObservers(t *testing.T) {
	first, second, observer := &ordinaryCountingHook{}, &ordinaryCountingHook{}, &ordinaryCountingHook{}
	m := NewManager(nil, nil, first)
	remove := m.AddHook(observer)
	m.SetHook(second)
	const n = 50
	var wg sync.WaitGroup
	wg.Go(func() {
		for range n {
			m.SetHook(first)
			m.SetHook(second)
		}
	})
	wg.Go(func() {
		for range n {
			m.hook.OnAuthRegistered(context.Background(), &Auth{ID: "fixture"})
		}
	})
	wg.Wait()
	if observer.registered.Load() != n || first.registered.Load()+second.registered.Load() != n {
		t.Fatal("a registration skipped or duplicated a primary or independent observer")
	}
	remove()
	m.SetHook(nil)
	m.hook.OnAuthRegistered(context.Background(), &Auth{ID: "fixture"})
	if observer.registered.Load() != n || first.registered.Load()+second.registered.Load() != n {
		t.Fatal("removed observers or old primary received a new event")
	}
}
