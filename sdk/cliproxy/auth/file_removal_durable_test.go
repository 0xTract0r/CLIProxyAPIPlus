package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDurableLocalRemovalFailureAndLateSave(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "ordinary.json")
	store := &fenceStore{make(chan struct{}), make(chan struct{})}
	m := NewManager(store, nil, nil)
	old, err := m.Register(ctx, fenceAuth(path, "original"))
	if err != nil {
		t.Fatal(err)
	}
	fail := errors.New("fixture cannot remove file")
	if err := m.RemoveDurable(ctx, []string{old.ID, old.ID}, func() error { return fail }); !errors.Is(err, fail) {
		t.Fatalf("commit failure: %v", err)
	}
	if got, ok := m.GetByID(old.ID); !ok || got.Generation != old.Generation || got.RegistrationEpoch != old.RegistrationEpoch {
		t.Fatal("failed commit retired or changed the generation")
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "original" {
		t.Fatalf("failed commit: %q %v", body, err)
	}
	late := old.Clone()
	late.Metadata["note"] = "hold"
	saved := make(chan error, 1)
	go func() { saved <- m.persist(ctx, late) }()
	select {
	case <-store.entered:
	case <-time.After(4 * time.Second):
		t.Fatal("existing store save did not start")
	}
	defer func() {
		select {
		case <-store.release:
		default:
			close(store.release)
		}
	}()
	deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := m.RemoveDurable(deadline, []string{old.ID}, func() error { t.Error("canceled callback ran"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("delete blocked on persist: %v", err)
	}
	if got, ok := m.GetByID(old.ID); !ok || got.RegistrationEpoch != old.RegistrationEpoch {
		t.Fatal("timeout retired record")
	}
	close(store.release)
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	finish, err := m.BeginFileRemoval(path)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	if err := m.RemoveDurable(ctx, []string{old.ID}, func() error { return os.Remove(path) }); err != nil {
		t.Fatal(err)
	}
	finish()
	if _, ok := m.GetByID(old.ID); ok {
		t.Fatal("runtime record survived")
	}
	if err := m.persist(ctx, old.Clone()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale write resurrected file: %v", err)
	}
	newer, err := m.Register(ctx, fenceAuth(path, "new epoch"))
	if err != nil || newer == nil || newer.RegistrationEpoch <= old.RegistrationEpoch {
		t.Fatalf("admission: %v", err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "new epoch" {
		t.Fatalf("new save: %q %v", body, err)
	}
	if err := m.persist(ctx, old.Clone()); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "new epoch" {
		t.Fatalf("old save replaced new: %q %v", body, err)
	}
}

func TestDurableLocalRemovalCompletesAfterSuccessfulCommitCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(&fenceStore{}, nil, nil)
	path := filepath.Join(t.TempDir(), "ordinary.json")
	a, err := m.Register(ctx, fenceAuth(path, "selected"))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.RemoveDurable(ctx, []string{a.ID}, func() error {
		if err := os.Remove(path); err != nil {
			return err
		}
		cancel()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.GetByID(a.ID); ok {
		t.Fatal("committed removal left a runtime record")
	}
	if err = m.persist(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale save after canceled commit recreated file")
	}
}

func TestRemoveIfRetiresAfterCanceledRequestAndPersistenceWait(t *testing.T) {
	m := NewManager(nil, nil, nil)
	id := "ordinary.json"
	if _, err := m.Register(context.Background(), &Auth{ID: id}); err != nil {
		t.Fatal(err)
	}

	persist := m.persistenceLock(id)
	if err := persist.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if persist != nil {
			persist.unlock()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	atGate := make(chan struct{})
	done := make(chan bool, 1)
	go func() {
		done <- m.RemoveIf(ctx, id, func(current *Auth) bool {
			close(atGate)
			return current != nil
		})
	}()
	select {
	case <-atGate:
	case <-time.After(4 * time.Second):
		t.Fatal("removal did not reach the credential gate")
	}
	persist.unlock()
	persist = nil
	select {
	case removed := <-done:
		if !removed {
			t.Fatal("request cancellation dropped committed retirement")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("removal did not complete after persistence was released")
	}
	if _, ok := m.GetByID(id); ok {
		t.Fatal("retired runtime record remains")
	}
	retry, stop := context.WithTimeout(context.Background(), 4*time.Second)
	defer stop()
	if _, err := m.Register(retry, &Auth{ID: id}); err != nil {
		t.Fatalf("same-ID registration after removal: %v", err)
	}
}
