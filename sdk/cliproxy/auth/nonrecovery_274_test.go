package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type nonRecovery274API interface {
	CommitNonRecoveryUpdate(context.Context, string, uint64, uint64, func(*Auth) (*Auth, error), func(context.Context, *Auth) error) (*Auth, error)
}

func commit274(m *Manager, ctx context.Context, id string, epoch, version uint64, prepare func(*Auth) (*Auth, error), save func(context.Context, *Auth) error) (*Auth, error) {
	if api, ok := any(m).(nonRecovery274API); ok {
		return api.CommitNonRecoveryUpdate(ctx, id, epoch, version, prepare, save)
	}
	a, _, err := m.Modify(ctx, id, epoch, func(current *Auth) error {
		next, err := prepare(current)
		if err == nil {
			*current = *next
		}
		return err
	})
	return a, err
}

type store274 struct {
	Store
	mu               sync.Mutex
	path             string
	saves            int
	fail             bool
	started, release chan struct{}
}

func (s *store274) Save(_ context.Context, a *Auth) (string, error) {
	s.mu.Lock()
	s.saves++
	fail, started, release := s.fail, s.started, s.release
	s.started = nil
	s.mu.Unlock()
	if started != nil {
		close(started)
		<-release
	}
	if fail {
		return "", errors.New("injected save failure")
	}
	raw, err := json.Marshal(a.Metadata)
	if err != nil {
		return "", err
	}
	return s.path, os.WriteFile(s.path, raw, 0600)
}

type hook274 struct {
	NoopHook
	mu    sync.Mutex
	calls int
	fn    func(context.Context, *Auth)
}

func (h *hook274) OnAuthUpdated(ctx context.Context, a *Auth) {
	h.mu.Lock()
	h.calls++
	fn := h.fn
	h.mu.Unlock()
	if fn != nil {
		fn(ctx, a)
	}
}
func setup274(t *testing.T) (*Manager, *store274, *hook274, *Auth) {
	t.Helper()
	s := &store274{path: filepath.Join(t.TempDir(), "auth.json")}
	h := &hook274{}
	m := NewManager(s, nil, h)
	a, err := m.Register(context.Background(), &Auth{ID: "nonrecovery274", Provider: "codex", FileName: "auth.json", Status: StatusQuarantined, Unavailable: true, AutoQuarantined: true, QuarantineReason: "terminal_auth_failure", QuarantinedAt: time.Now(), Disabled: true, Metadata: map[string]any{"type": "codex", "account_id": "account274", "email": "account274@example.invalid", "access_token": "old-access", "refresh_token": "old-refresh", "siwc": map[string]any{"access_token": "old-siwc", "refresh_token": "old-siwc-refresh"}, "note": "old-note", "reauth_required": true, "refresh_disabled": true, "auto_quarantined": true, "quota_refresh_status": "reauth_required", "disabled": true, "account_settings": map[string]any{"refresh_enabled": false}}})
	if err != nil {
		t.Fatal(err)
	}
	s.saves = 0
	return m, s, h, a
}
func TestCommitNonRecovery274Safety(t *testing.T) {
	for _, name := range []string{"metadata", "partial", "failed-save", "stale-epoch", "stale-version", "cross-account", "cross-provider"} {
		t.Run(name, func(t *testing.T) {
			m, s, h, before := setup274(t)
			rawBefore, _ := os.ReadFile(s.path)
			epoch, version := before.RegistrationEpoch, before.CredentialVersion
			if name == "failed-save" {
				s.fail = true
			}
			if name == "stale-epoch" {
				epoch++
			}
			if name == "stale-version" {
				version++
			}
			got, err := commit274(m, context.Background(), before.ID, epoch, version, func(a *Auth) (*Auth, error) {
				a.Metadata["note"] = "new-note"
				a.Status = StatusActive
				a.AutoQuarantined = false
				a.Unavailable = false
				a.Disabled = false
				a.Metadata["reauth_required"] = false
				a.Metadata["refresh_disabled"] = false
				a.Metadata["auto_quarantined"] = false
				a.Metadata["disabled"] = false
				a.Metadata["quota_refresh_status"] = "ok"
				a.Metadata["account_settings"] = map[string]any{"refresh_enabled": true}
				if name == "partial" {
					a.Metadata["access_token"] = "new-access"
				}
				if name == "cross-account" {
					a.Metadata["account_id"] = "other-account"
				}
				if name == "cross-provider" {
					a.Provider = "claude"
				}
				return a, nil
			}, func(ctx context.Context, a *Auth) error { _, err := s.Save(ctx, a); return err })
			rejection := name != "metadata" && name != "partial"
			live, _ := m.GetByID(before.ID)
			raw, _ := os.ReadFile(s.path)
			if rejection {
				if err == nil {
					t.Error("unsafe/failed transaction accepted")
				}
				if !reflect.DeepEqual(live, before) {
					t.Error("rejected transaction changed runtime")
				}
				if string(raw) != string(rawBefore) {
					t.Error("rejected transaction changed disk")
				}
				want := 0
				if name == "failed-save" {
					want = 1
				}
				if s.saves != want || h.calls != 0 {
					t.Errorf("rejection saves/hooks=%d/%d want %d/0", s.saves, h.calls, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.saves != 1 || h.calls != 1 {
				t.Errorf("saves/hooks=%d/%d", s.saves, h.calls)
			}
			if !got.AutoQuarantined || got.Status != before.Status || got.Unavailable != before.Unavailable || got.Disabled != before.Disabled || got.RegistrationEpoch != before.RegistrationEpoch || got.Index != before.Index || !got.CreatedAt.Equal(before.CreatedAt) {
				t.Error("non-recovery changed terminal/operator/history state")
			}
			wantVersion := before.CredentialVersion
			if name == "partial" {
				wantVersion++
			}
			if got.CredentialVersion != wantVersion {
				t.Errorf("version=%d want %d", got.CredentialVersion, wantVersion)
			}
			var disk map[string]any
			_ = json.Unmarshal(raw, &disk)
			for _, meta := range []map[string]any{got.Metadata, disk} {
				if meta["reauth_required"] != true || meta["refresh_disabled"] != true || meta["auto_quarantined"] != true || meta["disabled"] != true || meta["quota_refresh_status"] != "reauth_required" || meta["account_settings"].(map[string]any)["refresh_enabled"] != false {
					t.Error("non-recovery lost persisted recovery/operator/quota guards")
				}
				if meta["note"] != "new-note" {
					t.Error("ordinary metadata update lost")
				}
			}
		})
	}
}
func TestCommitNonRecovery274LateMarkResult(t *testing.T) {
	for _, changeToken := range []bool{false, true} {
		t.Run(map[bool]string{false: "metadata", true: "token"}[changeToken], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, s, _, old := setup274(t)
				s.started = make(chan struct{})
				started := s.started
				s.release = make(chan struct{})
				done := make(chan error, 1)
				go func() {
					_, err := commit274(m, context.Background(), old.ID, old.RegistrationEpoch, old.CredentialVersion, func(a *Auth) (*Auth, error) {
						a.Metadata["note"] = "committed-note"
						if changeToken {
							a.Metadata["access_token"] = "new-access"
						}
						return a, nil
					}, func(ctx context.Context, a *Auth) error { _, err := s.Save(ctx, a); return err })
					done <- err
				}()
				<-started
				resultStarted, resultDone := make(chan struct{}), make(chan struct{})
				go func() {
					close(resultStarted)
					m.MarkResult(context.Background(), Result{AuthID: old.ID, Provider: old.Provider, RegistrationEpoch: old.RegistrationEpoch, CredentialVersion: old.CredentialVersion, AccessToken: "old-access", Error: &Error{HTTPStatus: 429, Message: "late quota result"}})
					close(resultDone)
				}()
				<-resultStarted
				synctest.Wait()
				select {
				case <-resultDone:
					t.Error("MarkResult passed the same-account save gate")
				default:
				}
				close(s.release)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				<-resultDone
				live, _ := m.GetByID(old.ID)
				if live.Metadata["note"] != "committed-note" {
					t.Error("late result lost committed metadata")
				}
				if changeToken {
					if live.Failed != old.Failed || live.Metadata["access_token"] != "new-access" {
						t.Error("old credential result bypassed version fence")
					}
				} else if live.Failed != old.Failed+1 {
					t.Errorf("late valid result lost: failed=%d want %d", live.Failed, old.Failed+1)
				}
				raw, _ := os.ReadFile(s.path)
				var disk map[string]any
				_ = json.Unmarshal(raw, &disk)
				if disk["note"] != "committed-note" {
					t.Error("late result wrote a stale file snapshot")
				}
			})
		})
	}
}
func TestCommitNonRecovery274HooksOutsideGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, s, h, a := setup274(t)
		other, err := m.Register(context.Background(), &Auth{ID: "other274", Provider: "codex", Metadata: map[string]any{"account_id": "other274"}})
		if err != nil {
			t.Fatal(err)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		reentry := make(chan error, 1)
		sentinel := errors.New("entered edit")
		h.fn = func(ctx context.Context, got *Auth) {
			if got.ID != a.ID {
				return
			}
			bounded, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			_, _, err := m.Modify(bounded, got.ID, got.RegistrationEpoch, func(*Auth) error { return sentinel })
			reentry <- err
			close(entered)
			<-release
		}
		done := make(chan error, 1)
		go func() {
			_, err := commit274(m, context.Background(), a.ID, a.RegistrationEpoch, a.CredentialVersion, func(v *Auth) (*Auth, error) { v.Metadata["note"] = "hook-test"; return v, nil }, func(ctx context.Context, v *Auth) error { _, err := s.Save(ctx, v); return err })
			done <- err
		}()
		<-entered
		if err := <-reentry; !errors.Is(err, sentinel) {
			t.Errorf("hook retained own mutation gate: %v", err)
		}
		otherDone := make(chan error, 1)
		go func() {
			_, err := commit274(m, context.Background(), other.ID, other.RegistrationEpoch, other.CredentialVersion, func(v *Auth) (*Auth, error) { v.Metadata["note"] = "other-progress"; return v, nil }, func(context.Context, *Auth) error { return nil })
			otherDone <- err
		}()
		synctest.Wait()
		select {
		case err := <-otherDone:
			if err != nil {
				t.Error(err)
			}
		default:
			t.Error("unrelated auth blocked by hook")
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestCommitNonRecovery274StoreCannotRewriteManagerState(t *testing.T) {
	m, s, _, before := setup274(t)
	got, err := commit274(m, context.Background(), before.ID, before.RegistrationEpoch, before.CredentialVersion, func(a *Auth) (*Auth, error) { a.Metadata["note"] = "safe-save"; return a, nil }, func(ctx context.Context, a *Auth) error {
		_, err := s.Save(ctx, a)
		a.ID = "store-forged-id"
		a.RegistrationEpoch += 100
		a.CredentialVersion += 100
		a.Index = "store-forged-index"
		a.CreatedAt = time.Time{}
		a.Success += 100
		a.AutoQuarantined = false
		a.Disabled = false
		a.Status = StatusActive
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != before.ID || got.RegistrationEpoch != before.RegistrationEpoch || got.CredentialVersion != before.CredentialVersion || got.Index != before.Index || !got.CreatedAt.Equal(before.CreatedAt) || got.Success != before.Success || got.AutoQuarantined != before.AutoQuarantined || got.Disabled != before.Disabled || got.Status != before.Status {
		t.Errorf("store changed manager-owned state: id=%s epoch=%d version=%d index=%s created=%v success=%d quarantine=%t disabled=%t status=%s", got.ID, got.RegistrationEpoch, got.CredentialVersion, got.Index, got.CreatedAt, got.Success, got.AutoQuarantined, got.Disabled, got.Status)
	}
}
func TestCommitNonRecovery274RealTerminalState(t *testing.T) {
	m, s, h, old := setup274(t)
	fresh := old.Clone()
	fresh.Disabled = false
	fresh.Status = StatusActive
	fresh.AutoQuarantined = false
	fresh.Unavailable = false
	fresh.QuarantineReason = ""
	fresh.QuarantinedAt = time.Time{}
	fresh.Metadata["disabled"] = false
	for key := range fresh.Metadata {
		if isRefreshLockMetadataKey(key) || key == "auto_quarantined" || key == "quarantine_reason" || key == "quarantined_at" {
			delete(fresh.Metadata, key)
		}
	}
	fresh, err := m.RegisterReauthenticated(context.Background(), fresh)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		m.MarkResult(context.Background(), Result{AuthID: fresh.ID, Provider: fresh.Provider, RegistrationEpoch: fresh.RegistrationEpoch, CredentialVersion: fresh.CredentialVersion, AccessToken: "old-access", Error: &Error{HTTPStatus: 401, Message: "invalid authentication credentials"}})
	}
	before, _ := m.GetByID(fresh.ID)
	if !before.AutoQuarantined || !before.Unavailable || before.RejectedAccessToken != "old-access" {
		t.Fatalf("real MarkResult failed terminal precondition: quarantine=%t unavailable=%t rejected=%q", before.AutoQuarantined, before.Unavailable, before.RejectedAccessToken)
	}
	s.saves = 0
	h.calls = 0
	got, err := commit274(m, context.Background(), before.ID, before.RegistrationEpoch, before.CredentialVersion, func(a *Auth) (*Auth, error) {
		a.Metadata["note"] = "terminal-note"
		a.Metadata["access_token"] = "partial-new-access"
		a.AutoQuarantined = false
		a.Unavailable = false
		a.Status = StatusActive
		a.LastError = nil
		a.Quota = QuotaState{}
		return a, nil
	}, func(ctx context.Context, a *Auth) error { _, err := s.Save(ctx, a); return err })
	if err != nil {
		t.Fatal(err)
	}
	if !got.AutoQuarantined || !got.Unavailable || got.Status != before.Status || !reflect.DeepEqual(got.LastError, before.LastError) || !reflect.DeepEqual(got.Quota, before.Quota) || got.RejectedAccessToken != before.RejectedAccessToken || got.RegistrationEpoch != before.RegistrationEpoch || got.CredentialVersion != before.CredentialVersion+1 || s.saves != 1 || h.calls != 1 {
		t.Error("partial save released real terminal/quota state or broke transaction")
	}
}
