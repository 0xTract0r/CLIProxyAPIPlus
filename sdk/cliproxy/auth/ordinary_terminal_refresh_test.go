package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

type terminalRefreshHook struct {
	Hook
	updates atomic.Int32
}

func (h *terminalRefreshHook) OnAuthUpdated(context.Context, *Auth) { h.updates.Add(1) }

func TestOrdinaryTerminalRefreshPersistsAndNotifiesOnce(t *testing.T) {
	ctx := context.Background()
	store := &ordinaryRotationStore{}
	hook := &terminalRefreshHook{Hook: &NoopHook{}}
	manager := NewManager(store, nil, hook)
	const id = "terminal-fixture.json"
	if _, err := manager.Register(ctx, &Auth{
		ID: id, Provider: "synthetic",
		Metadata: map[string]any{"refresh_token": "local-grant", "expired": time.Now().Add(-time.Hour).Format(time.RFC3339), "note": "keep"},
	}); err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	failure := &Error{Code: "invalid_grant", Message: "invalid_grant", HTTPStatus: http.StatusBadRequest}
	manager.RegisterExecutor(&ordinaryRotationExecutor{refresh: func(*Auth) (*Auth, error) {
		attempts.Add(1)
		return nil, failure
	}})
	logger := logtest.NewGlobal()
	defer logger.Reset()
	var running sync.WaitGroup
	for range 2 {
		running.Go(func() {
			_, _ = manager.ForceRefreshAuth(ctx, id)
		})
	}
	running.Wait()
	if attempts.Load() != 1 || hook.updates.Load() != 1 {
		t.Fatalf("executor calls=%d, status notifications=%d", attempts.Load(), hook.updates.Load())
	}
	var notices int
	for _, entry := range logger.AllEntries() {
		if entry.Level == log.WarnLevel && entry.Message == "credential refresh requires manual sign-in" {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("terminal warnings=%d", notices)
	}
	cold := NewManager(store, nil, nil)
	if err := cold.Load(ctx); err != nil {
		t.Fatal(err)
	}
	a, ok := cold.GetByID(id)
	if !ok || !IsReauthRequiredMetadata(a.Metadata) || a.Metadata["note"] != "keep" {
		t.Fatal("cold load lost terminal marker or existing metadata")
	}
	if _, due := nextRefreshCheckAt(time.Now(), a, time.Minute); due {
		t.Fatal("cold terminal state was scheduled")
	}
}

type ordinaryRefreshThrottle struct{}

func (ordinaryRefreshThrottle) Error() string   { return "provider request rejected temporarily" }
func (ordinaryRefreshThrottle) StatusCode() int { return http.StatusTooManyRequests }
func (ordinaryRefreshThrottle) RetryAfter() *time.Duration {
	hint := 45 * time.Second
	return &hint
}

func TestOrdinaryRefreshRetainsProviderRetryHint(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(&ordinaryRotationStore{}, nil, nil)
	if _, err := manager.Register(ctx, &Auth{ID: "fixture", Provider: "synthetic", Metadata: map[string]any{"refresh_token": "local-grant"}}); err != nil {
		t.Fatal(err)
	}
	manager.RegisterExecutor(&ordinaryRotationExecutor{refresh: func(*Auth) (*Auth, error) { return nil, ordinaryRefreshThrottle{} }})
	_, err := manager.ForceRefreshAuth(ctx, "fixture")
	var throttle ordinaryRefreshThrottle
	if !errors.As(err, &throttle) || *throttle.RetryAfter() != 45*time.Second {
		t.Fatal("provider rejection or Retry-After lost")
	}
}

func TestOrdinaryTerminalRefreshCannotMarkReplacement(t *testing.T) {
	ctx := context.Background()
	store := &ordinaryRotationStore{}
	manager := NewManager(store, nil, nil)
	const id = "changed-while-refreshing.json"
	if _, err := manager.Register(ctx, &Auth{ID: id, Provider: "synthetic", Metadata: map[string]any{"refresh_token": "fixture-base"}}); err != nil {
		t.Fatal(err)
	}
	manager.RegisterExecutor(&ordinaryRotationExecutor{refresh: func(*Auth) (*Auth, error) {
		current, _ := manager.GetByID(id)
		current.Metadata["refresh_token"] = "fixture-replacement"
		if _, err := manager.Update(ctx, current); err != nil {
			return nil, err
		}
		return nil, &Error{Code: "invalid_grant", Message: "invalid_grant", HTTPStatus: http.StatusBadRequest}
	}})
	if _, err := manager.ForceRefreshAuth(ctx, id); !errors.Is(err, errVerifyCredentialSuperseded) {
		t.Fatalf("terminal refresh did not reject its old snapshot: %v", err)
	}
	cold := NewManager(store, nil, nil)
	if err := cold.Load(ctx); err != nil {
		t.Fatal(err)
	}
	a, ok := cold.GetByID(id)
	if !ok || a.Metadata["refresh_token"] != "fixture-replacement" || IsReauthRequiredMetadata(a.Metadata) {
		t.Fatal("terminal failure marked a concurrently replaced grant")
	}
}
