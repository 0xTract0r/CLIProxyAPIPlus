package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fenceStore struct {
	entered chan struct{}
	release chan struct{}
}

func (s *fenceStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (s *fenceStore) Delete(context.Context, string) error  { return nil }
func (s *fenceStore) Save(ctx context.Context, a *Auth) (string, error) {
	if a.Metadata["note"] == "hold" {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	path := a.Attributes[AttributePath]
	note := a.Metadata["note"].(string)
	return path, os.WriteFile(path, []byte(note), 0600)
}

func fenceAuth(path, note string) *Auth {
	return &Auth{ID: "ordinary.json", Attributes: map[string]string{AttributePath: path}, Metadata: map[string]any{"note": note}}
}

func TestFileFenceDefersFirstRegistrationAndRereadsAfterRemoval(t *testing.T) {
	ctx := WithSkipPersist(context.Background())
	m := NewManager(nil, nil, nil)
	dir := t.TempDir()
	path := filepath.Join(dir, "ordinary.json")
	release, err := m.BeginFileRemoval(dir + "/./ordinary.json")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := m.BeginFileRemoval(path); !errors.Is(err, ErrFileRemovalBusy) {
		t.Fatalf("concurrent removal: %v", err)
	}
	record := fenceAuth(path, "replacement")
	data := []byte(`{"note":"replacement"}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	record.BindFileSnapshot(path, data)
	_, err = m.Register(ctx, record)
	var blocked *FileRemovalInProgressError
	if !errors.As(err, &blocked) {
		t.Fatalf("registration: %v", err)
	}
	if _, ok := m.GetByID(record.ID); ok {
		t.Fatal("published during removal")
	}
	calls := 0
	retry := func() {
		calls++
		if _, err := m.Register(ctx, record.Clone()); err != nil {
			t.Errorf("reentrant registration: %v", err)
		}
	}
	blocked.AfterRemoval(record, retry)
	blocked.AfterRemoval(record.Clone(), retry)
	if calls != 0 {
		t.Fatal("premature callback")
	}
	release()
	release()
	if calls != 1 {
		t.Fatalf("replays=%d", calls)
	}
	if got, ok := m.GetByID(record.ID); !ok || got.Metadata["note"] != "replacement" {
		t.Fatal("replacement was not registered")
	}
}

func TestFileFenceProtectsRegistrationWhileStoreSaves(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	store := &fenceStore{make(chan struct{}), make(chan struct{})}
	m := NewManager(store, nil, nil)
	path := filepath.Join(t.TempDir(), "ordinary.json")
	record := fenceAuth(path, "hold")
	record.Attributes[AttributeVirtualSource] = path
	done := make(chan error, 1)
	go func() { _, err := m.Register(ctx, record); done <- err }()
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	m.mu.RLock()
	count := m.registeringFiles[path]
	m.mu.RUnlock()
	if count != 1 {
		t.Errorf("path aliases reserved %d times", count)
	}
	if _, err := m.BeginFileRemoval(path); !errors.Is(err, ErrFileRemovalBusy) {
		t.Errorf("removal: %v", err)
	}
	close(store.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	release, err := m.BeginFileRemoval(path)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestFileFenceRejectsRetargetButKeepsOrdinaryUpdate(t *testing.T) {
	ctx := context.Background()
	m := NewManager(&fenceStore{}, nil, nil)
	dir := t.TempDir()
	a, b := filepath.Join(dir, "first.json"), filepath.Join(dir, "second.json")
	record, err := m.Register(ctx, fenceAuth(a, "initial"))
	if err != nil {
		t.Fatal(err)
	}
	for _, reserved := range []string{a, b} {
		release, err := m.BeginFileRemoval(reserved)
		if err != nil {
			t.Fatal(err)
		}
		next := record.Clone()
		next.Attributes[AttributePath] = b
		_, err = m.Update(ctx, next)
		var blocked *FileRemovalInProgressError
		if !errors.As(err, &blocked) {
			t.Errorf("retarget while %s reserved: %v", filepath.Base(reserved), err)
		}
		if got, _ := m.GetByID(record.ID); got.Attributes[AttributePath] != a {
			t.Error("rejected update published")
		}
		if _, err := os.Stat(b); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("rejected update touched destination: %v", err)
		}
		release()
	}
	release, err := m.BeginFileRemoval(a)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	regular := record.Clone()
	regular.Metadata["note"] = "ordinary edit"
	if _, err := m.Update(ctx, regular); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(a)
	if err != nil || string(saved) != "ordinary edit" {
		t.Fatalf("same source write: %q, %v", saved, err)
	}
	m.Remove(ctx, record.ID)
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Update(ctx, regular); got != nil || err != nil {
		t.Fatalf("retired record update: %v, %v", got != nil, err)
	}
	if _, err := os.Stat(a); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("retired update recreated source: %v", err)
	}
}

func TestFileFenceRejectsStaleFirstRegistration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordinary.json")
	old := []byte(`{"note":"old value"}`)
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	observed := &Auth{ID: "ordinary.json", Attributes: map[string]string{AttributePath: path}, Metadata: map[string]any{"note": "old value"}}
	observed.BindFileSnapshot(path, old)
	if err := os.WriteFile(path, []byte(`{"note":"replacement"}`), 0600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(nil, nil, nil)
	if registered, err := manager.Register(WithSkipPersist(context.Background()), observed); err == nil || registered != nil {
		t.Fatalf("stale first registration: committed=%v err=%v", registered != nil, err)
	}
	if _, ok := manager.GetByID(observed.ID); ok {
		t.Fatal("stale snapshot entered the manager")
	}
}

func TestFileFenceReleasesAfterCancellationAndStoreFailure(t *testing.T) {
	m := NewManager(&fenceStore{}, nil, nil)
	path := filepath.Join(t.TempDir(), "missing", "ordinary.json")
	a := fenceAuth(path, "ordinary")
	releaseID, err := m.lockAuthMutationContext(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseID()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := m.Register(ctx, a); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled registration: returned=%v err=%v", got != nil, err)
	}
	releaseID()
	if got, err := m.Register(context.Background(), a); got == nil || err != nil {
		t.Fatalf("nonfatal Store.Save behavior changed: returned=%v err=%v", got != nil, err)
	}
	releasePath, err := m.BeginFileRemoval(path)
	if err != nil {
		t.Fatalf("reservation leaked after cancellation or Store.Save failure: %v", err)
	}
	releasePath()
}
