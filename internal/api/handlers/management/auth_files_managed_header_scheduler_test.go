package management

import (
	"testing"
	"time"
)

func TestManagedHeaderSyncScheduler_ShouldEnqueueRespectsInFlight(t *testing.T) {
	scheduler := newManagedHeaderSyncScheduler()
	now := time.Now()

	if !scheduler.shouldEnqueue("auth-1", now) {
		t.Fatalf("first enqueue should succeed")
	}
	// Second concurrent attempt must be deduped while the first is in flight.
	if scheduler.shouldEnqueue("auth-1", now) {
		t.Fatalf("second enqueue should be rejected while in-flight")
	}
	scheduler.recordSuccess("auth-1", now)
	// Immediately after success, the cooldown still blocks re-enqueue.
	if scheduler.shouldEnqueue("auth-1", now.Add(time.Second)) {
		t.Fatalf("enqueue should be rejected during success cooldown")
	}
	// After the cooldown expires the scheduler must permit a new attempt.
	if !scheduler.shouldEnqueue("auth-1", now.Add(managedHeaderSyncCooldownOnSuccess+time.Second)) {
		t.Fatalf("enqueue should be allowed after cooldown elapsed")
	}
}

func TestManagedHeaderSyncScheduler_FailureBackoffGrows(t *testing.T) {
	scheduler := newManagedHeaderSyncScheduler()
	now := time.Now()

	if !scheduler.shouldEnqueue("auth-2", now) {
		t.Fatalf("first enqueue should succeed")
	}
	scheduler.recordFailure("auth-2", now)

	// After the initial failure backoff window we should be eligible again.
	if !scheduler.shouldEnqueue("auth-2", now.Add(managedHeaderSyncCooldownInitialFailure+time.Second)) {
		t.Fatalf("enqueue should be allowed after initial failure cooldown")
	}
	scheduler.recordFailure("auth-2", now.Add(managedHeaderSyncCooldownInitialFailure+time.Second))

	// Second failure should at least double the cooldown.
	if scheduler.shouldEnqueue("auth-2", now.Add(managedHeaderSyncCooldownInitialFailure*2)) {
		t.Fatalf("enqueue should be rejected during expanded backoff")
	}
}

func TestManagedHeaderSyncScheduler_ClearResetsState(t *testing.T) {
	scheduler := newManagedHeaderSyncScheduler()
	now := time.Now()

	if !scheduler.shouldEnqueue("auth-3", now) {
		t.Fatalf("first enqueue should succeed")
	}
	scheduler.recordFailure("auth-3", now)
	scheduler.clear("auth-3")

	if !scheduler.shouldEnqueue("auth-3", now) {
		t.Fatalf("enqueue should be allowed immediately after clear")
	}
}
