package executor

import (
	"context"
	"errors"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type continuation361Wait struct {
	context.Context
	once             sync.Once
	observed, resume chan struct{}
}

func (c *continuation361Wait) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed); <-c.resume })
	return c.Context.Done()
}

type continuation361Entered struct {
	context.Context
	once    sync.Once
	entered chan struct{}
}

func (c *continuation361Entered) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.entered) })
	return err
}
func TestContinuation361WaitGeneration(t *testing.T) {
	for _, mode := range []string{"close-reopen-before-wake", "fence-held-cancel"} {
		t.Run(mode, func(t *testing.T) {
			done := make(chan struct{})
			up := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := up.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer c.Close()
				<-done
			}))
			t.Cleanup(func() { close(done); server.Close() })
			cfg := &config.Config{}
			cfg.ProxyURL = "http://old-global.invalid"
			e := NewCodexWebsocketsExecutor(cfg)
			e.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			a := &auth.Auth{ID: "same-auth", Provider: "codex", RegistrationEpoch: 9}
			url := "wss://same.invalid/responses"
			install := func() *codexWebsocketSession {
				c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c.Close() })
				s := e.getOrCreateSession("same-logical", a, url, cfg.ProxyURL)
				s.conn = c
				s.connCloser = newWebsocketConnectionCloser(c)
				s.connCfg = cfg
				s.hasConnCfg = true
				s.proxyURL = cfg.ProxyURL
				s.connEpoch = 9
				s.connProvider = "codex"
				s.bindDisconnectOwner(c)
				return s
			}
			old := install()
			ctx := exec.WithRequiredUpstreamWebsocket(context.Background())
			type result struct {
				lease *codexWebsocketSessionLease
				err   error
			}
			finished := make(chan result, 1)
			if mode == "close-reopen-before-wake" {
				old.reqMu.Lock()
				defer old.reqMu.Unlock()
				observed, resume := make(chan struct{}), make(chan struct{})
				waitctx := &continuation361Wait{Context: ctx, observed: observed, resume: resume}
				go func() {
					l, err := e.acquireSessionLease(waitctx, "same-logical", a, url, false)
					finished <- result{l, err}
				}()
				select {
				case <-observed:
				case <-time.After(time.Second):
					t.Fatal("waiter never reached lease wait")
				}
				e.CloseExecutionSession("same-logical")
				next := install()
				if next == old || next.disconnectOwner == old.disconnectOwner {
					t.Fatal("probe did not replace owner/physical generation")
				}
				close(resume)
				select {
				case r := <-finished:
					if r.lease != nil {
						r.lease.release()
					}
					if r.err == nil {
						t.Fatal("waiting old continuation borrowed reopened connection")
					}
				case <-time.After(time.Second):
					t.Fatal("waiter failed to finish")
				}
			} else {
				owner := old.disconnectOwner
				owner.interruptFence.Lock()
				canceled, cancel := context.WithCancel(ctx)
				defer cancel()
				entered := make(chan struct{})
				marked := &continuation361Entered{Context: canceled, entered: entered}
				go func() {
					l, err := e.acquireSessionLease(marked, "same-logical", a, url, false)
					finished <- result{l, err}
				}()
				<-entered
				cancel()
				select {
				case r := <-finished:
					owner.interruptFence.Unlock()
					if r.lease != nil {
						r.lease.release()
					}
					if !errors.Is(r.err, context.Canceled) {
						t.Fatalf("expected cancellation: %v", r.err)
					}
				case <-time.After(250 * time.Millisecond):
					owner.interruptFence.Unlock()
					r := <-finished
					if r.lease != nil {
						r.lease.release()
					}
					t.Fatal("cancellation waited for held owner fence")
				}
			}
		})
	}
}
