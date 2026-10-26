package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCommitReauthenticatedTransaction223(t *testing.T) {
	for _, mode := range []string{"success", "persist-failure", "stale-epoch"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			m := NewManager(nil, nil, nil)
			old, _ := m.Register(ctx, &Auth{ID: "tx223", Provider: "codex", AutoQuarantined: true, Metadata: map[string]any{"account_id": "a", "access_token": "old"}})
			_, _, err := m.Modify(ctx, old.ID, old.RegistrationEpoch, func(a *Auth) error { a.Metadata["note"] = "latest"; return nil })
			if err != nil {
				t.Fatal(err)
			}
			expected := old.RegistrationEpoch
			if mode == "stale-epoch" {
				next := old.Clone()
				_, err = m.RegisterReauthenticated(WithSkipPersist(ctx), next)
				if err != nil {
					t.Fatal(err)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			calls := 0
			go func() {
				_, err := m.CommitReauthenticated(ctx, old.ID, expected, func(current *Auth) (*Auth, error) {
					if current.Metadata["note"] != "latest" {
						return nil, errors.New("lost latest ordinary edit")
					}
					current.Metadata["access_token"] = "new"
					current.ClearAutoQuarantine()
					return current, nil
				}, func(ctx context.Context, a *Auth) error {
					calls++
					close(entered)
					<-release
					if mode == "persist-failure" {
						return errors.New("save failed")
					}
					return nil
				})
				done <- err
			}()
			if mode == "stale-epoch" {
				if err := <-done; err == nil || calls != 0 {
					t.Fatal("stale epoch reached persistence")
				}
				return
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("transaction did not reach persistence")
			}
			observed, _ := m.GetByID(old.ID)
			if observed.RegistrationEpoch != old.RegistrationEpoch || observed.Metadata["access_token"] != "old" || !observed.AutoQuarantined {
				t.Fatal("published before persistence")
			}
			waitCtx, cancelWait := context.WithTimeout(ctx, 50*time.Millisecond)
			_, _, waitErr := m.Modify(waitCtx, old.ID, expected, func(a *Auth) error { return errors.New("same-account edit escaped the gate") })
			cancelWait()
			if !errors.Is(waitErr, context.DeadlineExceeded) {
				t.Fatalf("same-account gate wait = %v", waitErr)
			}
			editStarted, editDone := make(chan struct{}), make(chan error, 1)
			go func() {
				close(editStarted)
				_, _, err := m.Modify(ctx, old.ID, expected, func(a *Auth) error { a.Metadata["note"] = "after"; return nil })
				editDone <- err
			}()
			<-editStarted
			otherDone := make(chan error, 1)
			go func() { _, err := m.Register(ctx, &Auth{ID: "other223", Provider: "codex"}); otherDone <- err }()
			select {
			case err := <-otherDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("unrelated account blocked by store I/O")
			}
			select {
			case <-editDone:
				t.Fatal("same-account edit escaped credential gate")
			default:
			}
			close(release)
			txErr := <-done
			editErr := <-editDone
			live, _ := m.GetByID(old.ID)
			if mode == "persist-failure" {
				if txErr == nil || editErr != nil || live.RegistrationEpoch != old.RegistrationEpoch || live.Metadata["access_token"] != "old" || !live.AutoQuarantined {
					t.Fatal("failed save changed registration")
				}
				return
			}
			if txErr != nil || editErr == nil || live.RegistrationEpoch <= old.RegistrationEpoch || live.Metadata["access_token"] != "new" || live.Metadata["note"] != "latest" {
				t.Fatalf("transaction/fencing failed: tx=%v edit=%v", txErr, editErr)
			}
		})
	}
}
