package cliproxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

func TestProbe254ServiceOwnershipAndShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetAntigravityCapabilityCache()
		defer resetAntigravityCapabilityCache()
		ctxA, cancelA := context.WithCancel(context.Background())
		defer cancelA()
		ctxB, cancelB := context.WithCancel(context.Background())
		defer cancelB()
		started, cancelled, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		previous := http.DefaultTransport
		defer func() { http.DefaultTransport = previous }()
		http.DefaultTransport = antigravityReviewTransport(func(r *http.Request) (*http.Response, error) {
			close(started)
			<-r.Context().Done()
			close(cancelled)
			<-release
			close(exited)
			return nil, r.Context().Err()
		})
		a := &Service{cfg: &config.Config{}, antigravityContext: ctxA, runCancel: cancelA}
		b := &Service{cfg: &config.Config{}, antigravityContext: ctxB, runCancel: cancelB}
		auth := antigravityTestAuth("same-shared-254", "https://version.invalid")
		a.asyncProbeAntigravityCapabilities(ctxA, auth, "antigravity")
		synctest.Wait()
		<-started
		b.asyncProbeAntigravityCapabilities(ctxB, auth, "antigravity")
		synctest.Wait()
		b.StopAntigravityProbes()
		bDone := make(chan struct{})
		go func() { b.WaitAntigravityProbes(); close(bDone) }()
		synctest.Wait()
		select {
		case <-bDone:
		default:
			t.Error("follower Service barrier waited for another Service's leader")
		}
		select {
		case <-cancelled:
			t.Error("follower Service stop cancelled another Service leader")
		default:
		}
		aDone := make(chan error, 1)
		go func() { aDone <- a.Shutdown(context.Background()) }()
		synctest.Wait()
		<-cancelled
		select {
		case err := <-aDone:
			t.Errorf("Shutdown returned before owned HTTP exit: %v", err)
		default:
		}
		close(release)
		synctest.Wait()
		<-exited
		if err := <-aDone; err != nil {
			t.Fatal(err)
		}
		<-bDone
		synctest.Wait()
	})
}
func TestProbe254ClosedAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetAntigravityCapabilityCache()
		defer resetAntigravityCapabilityCache()
		previous := http.DefaultTransport
		defer func() { http.DefaultTransport = previous }()
		http.DefaultTransport = antigravityReviewTransport(func(*http.Request) (*http.Response, error) {
			t.Error("closed Service admitted HTTP")
			return nil, context.Canceled
		})
		s := &Service{cfg: &config.Config{}}
		s.StopAntigravityProbes()
		auth := antigravityTestAuth("closed-254", "https://version.invalid")
		s.asyncProbeAntigravityCapabilities(context.Background(), auth, "antigravity")
		s.fetchAntigravityModelCapabilityHints(context.Background(), auth)
		s.WaitAntigravityProbes()
		synctest.Wait()
	})
}

// Install a paused real singleflight leader, then let the production fetch path
// join it. This controls the pre-start window without scheduler timing or hooks.
func TestProbe254CancelledLeaderPreservesFollowerFallback(t *testing.T) {
	for _, abandoned := range []bool{true, false} {
		t.Run(fmt.Sprintf("abandoned_%t", abandoned), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				resetAntigravityCapabilityCache()
				defer resetAntigravityCapabilityCache()
				previous := http.DefaultTransport
				defer func() { http.DefaultTransport = previous }()
				http.DefaultTransport = antigravityReviewTransport(func(*http.Request) (*http.Response, error) {
					t.Error("cancelled pre-start leader unexpectedly reached HTTP")
					return nil, context.Canceled
				})
				owner, follower := &Service{cfg: &config.Config{}}, &Service{cfg: &config.Config{}}
				auth := antigravityTestAuth("fallback-254", "https://version.invalid")
				key := follower.antigravityCapabilityKey(auth)
				token, _ := auth.Metadata["access_token"].(string)
				flightKey := key + fmt.Sprintf("/%x", sha256.Sum256([]byte(token)))
				antigravityCapabilityMu.Lock()
				antigravityCapabilityCache[key] = antigravityCapabilityCacheEntry{
					hints:     antigravityModelCapabilityHints{ModelIDs: map[string]struct{}{"preserved-stale-model": {}}, WebSearchModelIDs: map[string]struct{}{"preserved-search-model": {}}, revision: 51},
					expiresAt: antigravityNowFunc().Add(-time.Hour),
				}
				antigravityCapabilityMu.Unlock()
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				leaderCtx, ticket := owner.reserveAntigravityProbe(parent)
				if abandoned {
					ticket.abandon()
				} else {
					cancel()
					defer ticket.abandon()
				}
				started, release := make(chan struct{}), make(chan struct{})
				leaderResult := antigravityCapabilityGroup.DoChan(flightKey, func() (any, error) {
					close(started)
					<-release
					if active, err := ticket.start(leaderCtx); !active {
						return antigravityModelCapabilityHints{}, err
					}
					defer ticket.finish()
					t.Error("cancelled reservation unexpectedly admitted a leader")
					return antigravityModelCapabilityHints{}, nil
				})
				<-started
				result := make(chan antigravityModelCapabilityHints, 1)
				go func() {
					_, hints := follower.fetchAntigravityModelCapabilityHints(context.Background(), auth)
					result <- hints
				}()
				synctest.Wait()
				select {
				case <-result:
					t.Error("expired cache returned before joining the paused leader")
				default:
				}
				close(release)
				synctest.Wait()
				got := <-result
				shared := <-leaderResult
				owner.WaitAntigravityProbes()
				follower.WaitAntigravityProbes()
				if !shared.Shared {
					t.Error("production fetch did not join the same singleflight leader")
				}
				if _, ok := got.ModelIDs["preserved-stale-model"]; !ok {
					t.Errorf("active follower lost stale model hints: %#v", got)
				}
				if _, ok := got.WebSearchModelIDs["preserved-search-model"]; !ok {
					t.Errorf("active follower lost stale search hints: %#v", got)
				}
				if got.revision != 51 {
					t.Errorf("stale revision = %d, want 51", got.revision)
				}
			})
		})
	}
}
