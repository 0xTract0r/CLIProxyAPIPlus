package cliproxy

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"net/http"
	"testing"
	"testing/synctest"
)

// 确定性持有singleflight HTTP后代直到外层Wait返回；不sleep、不改race开关。
func TestProbe248CancellationWaitMustJoinHTTPDescendant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetAntigravityCapabilityCache()
		defer resetAntigravityCapabilityCache()
		lifetime, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		cancelled := make(chan struct{})
		release := make(chan struct{})
		exited := make(chan struct{})
		previous := http.DefaultTransport
		http.DefaultTransport = antigravityReviewTransport(func(req *http.Request) (*http.Response, error) {
			close(started)
			<-req.Context().Done()
			close(cancelled)
			<-release
			close(exited)
			return nil, req.Context().Err()
		})
		defer func() { http.DefaultTransport = previous }()
		svc := &Service{cfg: &config.Config{}, antigravityContext: lifetime}
		auth := antigravityTestAuth("probe-lifecycle248", "https://version.invalid")
		svc.asyncProbeAntigravityCapabilities(lifetime, auth, "antigravity")
		synctest.Wait()
		select {
		case <-started:
		default:
			t.Fatal("controlled HTTP descendant never started")
		}
		cancel()
		synctest.Wait()
		select {
		case <-cancelled:
		default:
			t.Fatal("lifetime cancellation did not reach HTTP descendant")
		}
		joined := make(chan struct{})
		go func() { svc.WaitAntigravityProbes(); close(joined) }()
		synctest.Wait()
		select {
		case <-joined:
			select {
			case <-exited:
				t.Fatal("release barrier unexpectedly bypassed")
			default:
			}
			t.Error("quiescence violated: WaitAntigravityProbes returned while cancelled singleflight HTTP descendant is still active")
		default:
			t.Log("Wait correctly remains blocked until HTTP descendant exits")
		}
		close(release)
		synctest.Wait()
		<-exited
		<-joined
		synctest.Wait()
		t.Log("controlled descendant released and all bubble goroutines joined before restoring globals")
	})
}
