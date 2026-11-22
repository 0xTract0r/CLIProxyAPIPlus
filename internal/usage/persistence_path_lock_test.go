package usage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestUsagePathLockDoesNotBlockOtherHistory(t *testing.T) {
	first := filepath.Join(t.TempDir(), "history.json")
	second := filepath.Join(t.TempDir(), "history.json")
	a, b := NewRequestStatistics(), NewRequestStatistics()
	if err := a.SetPersistencePath(first); err != nil {
		t.Fatal(err)
	}
	if err := b.SetPersistencePath(second); err != nil {
		t.Fatal(err)
	}
	held := flock.New(first + ".lock")
	if err := held.Lock(); err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	waiting := make(chan error, 1)
	go func() { waiting <- a.SaveToPersistence() }()
	select {
	case err := <-waiting:
		t.Fatalf("locked history returned early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	other := make(chan error, 1)
	go func() { other <- b.SaveToPersistence() }()
	select {
	case err := <-other:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("separate history blocked on another history's sidecar")
	}
	if err := held.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := <-waiting; err != nil {
		t.Fatal(err)
	}
}

func TestUsageIndependentProcessCommit(t *testing.T) {
	const childEnv = "CLIPROXY_USAGE_TEST_CHILD"
	if id := os.Getenv(childEnv); id != "" {
		dir := os.Getenv("CLIPROXY_USAGE_TEST_DIRECTORY")
		stats := NewRequestStatistics()
		if err := stats.SetPersistencePath(filepath.Join(dir, "history.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".ready"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		tokenCount := int64(1)
		if id == "fixture-b" {
			tokenCount = 2
		}
		detail := RequestDetail{
			RequestID: id, Timestamp: time.Unix(1780000000, 0).UTC(),
			Tokens: TokenStats{InputTokens: tokenCount},
		}
		snapshot := StatisticsSnapshot{APIs: map[string]APISnapshot{
			"synthetic": {Models: map[string]ModelSnapshot{"fixture": {Details: []RequestDetail{detail}}}},
		}}
		_, err := stats.ImportAndSave(snapshot)
		outcome := "committed"
		if errors.Is(err, ErrUsageSnapshotChanged) {
			outcome = "stale"
		} else if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".result"), []byte(outcome), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}

	dir := t.TempDir()
	history := filepath.Join(dir, "history.json")
	if err := os.WriteFile(history, []byte(`{"version":2,"usage":{"apis":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(history + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	spawn := func(id string) func() {
		t.Helper()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUsageIndependentProcessCommit$")
		cmd.Env = append(os.Environ(), childEnv+"="+id, "CLIPROXY_USAGE_TEST_DIRECTORY="+dir)
		var log bytes.Buffer
		cmd.Stdout, cmd.Stderr = &log, &log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		wait := sync.OnceValue(cmd.Wait)
		t.Cleanup(func() { _ = wait() })
		for {
			if _, err := os.Stat(filepath.Join(dir, id+".ready")); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatalf("child did not open history: %s", id)
			case <-time.After(5 * time.Millisecond):
			}
		}
		return func() {
			t.Helper()
			if err := wait(); err != nil {
				t.Fatalf("child %s: %v\n%s", id, err, &log)
			}
		}
	}
	waitA, waitB := spawn("fixture-a"), spawn("fixture-b")
	// Both independent processes have read the old file. A running writer
	// must wait on the sidecar held by this process before it can commit.
	time.Sleep(100 * time.Millisecond)
	for _, id := range []string{"fixture-a", "fixture-b"} {
		if _, err := os.Stat(filepath.Join(dir, id+".result")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("child bypassed the sidecar: %s, %v", id, err)
		}
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	waitA()
	waitB()
	readOutcome := func(id string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(dir, id+".result"))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	a, b := readOutcome("fixture-a"), readOutcome("fixture-b")
	if (a != "committed" || b != "stale") && (a != "stale" || b != "committed") {
		t.Fatalf("want one committed and one stale, got %s and %s", a, b)
	}
	loser := "fixture-b"
	if a == "stale" {
		loser = "fixture-a"
	}
	for _, suffix := range []string{".ready", ".result"} {
		if err := os.Remove(filepath.Join(dir, loser+suffix)); err != nil {
			t.Fatal(err)
		}
	}
	spawn(loser)()
	if got := readOutcome(loser); got != "committed" {
		t.Fatalf("cold retry returned %s", got)
	}
	reopened := NewRequestStatistics()
	if err := reopened.SetPersistencePath(history); err != nil {
		t.Fatal(err)
	}
	snapshot := reopened.Snapshot()
	details := snapshot.APIs["synthetic"].Models["fixture"].Details
	ids := map[string]bool{}
	for _, detail := range details {
		ids[detail.RequestID] = true
	}
	if snapshot.TotalRequests != 2 || !ids["fixture-a"] || !ids["fixture-b"] {
		t.Fatalf("cold reopen got %d requests and IDs %v", snapshot.TotalRequests, ids)
	}
}
