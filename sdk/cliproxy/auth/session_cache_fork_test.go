package auth

import (
	"testing"
	"time"
)

func TestSessionAliasesKeepWarmupServingOnTouch(t *testing.T) {
	cache := NewSessionCache(time.Hour)
	defer cache.Stop()
	cache.SetAliases("auth", "primary", "fallback")
	state := &warmupServingSession{reserved: true, revision: 7}
	cache.mu.Lock()
	cache.setAliasGroupServingLocked("primary", state)
	exits := 0
	cache.onServingExit = func(string, sessionEntry) { exits++ }
	cache.mu.Unlock()
	if !cache.Touch("fallback", "auth") {
		t.Fatal("touch failed")
	}
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	if exits != 0 {
		t.Fatalf("Touch released %d pacing bindings", exits)
	}
	for _, key := range []string{"primary", "fallback"} {
		entry := cache.entries[key]
		if entry.serving != state {
			t.Fatalf("alias %s lost serving reservation", key)
		}
		if cache.groups[entry.aliases[0]].serving != state {
			t.Fatal("group index lost reservation")
		}
	}
}

func TestSessionAliasInvalidationPreservesSurvivingWarmupAlias(t *testing.T) {
	cache := NewSessionCache(time.Hour)
	defer cache.Stop()
	cache.SetAliases("auth", "primary", "fallback")
	state := &warmupServingSession{reserved: true, revision: 7}
	cache.mu.Lock()
	cache.setAliasGroupServingLocked("primary", state)
	exits := make(map[string]int)
	cache.onServingExit = func(key string, _ sessionEntry) { exits[key]++ }
	cache.mu.Unlock()
	cache.Invalidate("fallback")
	if got, ok := cache.Get("primary"); !ok || got != "auth" {
		t.Fatal("primary binding lost")
	}
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	if exits["fallback"] != 1 || exits["primary"] != 0 {
		t.Fatalf("unexpected exits: %v", exits)
	}
	if cache.entries["primary"].serving != state {
		t.Fatal("surviving reservation lost")
	}
	if _, ok := cache.servingExpired["primary"]; ok {
		t.Fatal("active alias recorded as expired")
	}
}
