package usage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestClosedUsageStoreCannotOverwriteReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	open := func() *RequestStatistics {
		s := NewRequestStatistics()
		if err := s.SetPersistencePath(path); err != nil {
			t.Fatal(err)
		}
		return s
	}
	event := func(n int64) coreusage.Record {
		return coreusage.Record{Provider: "fixture", Model: "sample", RequestedAt: time.Unix(n, 0), Detail: coreusage.Detail{TotalTokens: 2}}
	}
	old := open()
	old.HandleUsage(context.Background(), event(1))
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	next := open()
	next.HandleUsage(context.Background(), event(2))
	if err := next.Close(); err != nil {
		t.Fatal(err)
	}
	// An old request or a previously queued persistence callback both arrive late.
	old.HandleUsage(context.Background(), event(3))
	if err := old.SaveToPersistence(); err != nil {
		t.Fatal(err)
	}
	restored := open()
	if n := restored.Snapshot().TotalRequests; n != 2 {
		t.Fatalf("reopened history contains %d events", n)
	}
}
func TestRequestObserverSurvivesOtherDispatcherShutdown(t *testing.T) {
	owner := NewRequestStatistics()
	dispatcher := coreusage.NewManager(1)
	dispatcher.Stop()
	ctx := coreusage.WithRequestObserver(context.Background(), owner)
	dispatcher.Publish(ctx, coreusage.Record{Model: "sample", APIKey: "private-fixture"})
	if got := owner.Snapshot().TotalRequests; got != 1 {
		t.Fatalf("active request owner collected %d events", got)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	dispatcher.Publish(ctx, coreusage.Record{Model: "sample"})
	if got := owner.Snapshot().TotalRequests; got != 1 {
		t.Fatalf("closed owner received a late event: %d", got)
	}
}
