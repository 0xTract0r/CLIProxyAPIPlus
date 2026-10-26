package executor

import (
	"context"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestContinuation356LeaseBoundaries(t *testing.T) {
	for _, mode := range []string{"global-only", "epoch", "account", "provider", "endpoint", "profile", "account-proxy", "request-proxy", "close-waiter"} {
		t.Run(mode, func(t *testing.T) {
			done := make(chan struct{})
			up := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, e := up.Upgrade(w, r, nil)
				if e != nil {
					return
				}
				defer c.Close()
				<-done
			}))
			t.Cleanup(func() { close(done); server.Close() })
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			cfg := &config.Config{}
			cfg.ProxyURL = "http://old-global.invalid"
			e := NewCodexWebsocketsExecutor(cfg)
			e.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			a := &auth.Auth{ID: "a", Provider: "codex", RegistrationEpoch: 7}
			url := "wss://scope.invalid/responses"
			s := e.getOrCreateSession("scope", a, url, cfg.ProxyURL)
			s.conn = conn
			s.connCloser = newWebsocketConnectionCloser(conn)
			s.connCfg = cfg
			s.hasConnCfg = true
			s.proxyURL = cfg.ProxyURL
			s.connEpoch = 7
			s.connProvider = "codex"
			s.bindDisconnectOwner(conn)
			next := &config.Config{}
			next.ProxyURL = "http://new-global.invalid"
			e.cfg = next
			ctx := exec.WithRequiredUpstreamWebsocket(context.Background())
			switch mode {
			case "epoch":
				a.RegistrationEpoch++
			case "account":
				a.ID = "b"
			case "provider":
				a.Provider = "other"
			case "endpoint":
				url = "wss://other.invalid/responses"
			case "profile":
				a.Metadata = map[string]any{"account_settings": map[string]any{"transport_profile": map[string]any{"preset": "provider-default"}, "tls_profile": map[string]any{"preset": "codex_go_http11_v1", "force_http11": true}}}
			case "account-proxy":
				a.ProxyURL = "direct"
			case "request-proxy":
				ctx = exec.WithRequestProxyURL(ctx, "direct")
			}
			if mode == "close-waiter" {
				s.reqMu.Lock()
				defer s.reqMu.Unlock()
				bounded, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				result := make(chan error, 1)
				go func() {
					lease, err := e.acquireSessionLease(bounded, "scope", a, url, false)
					if lease != nil {
						lease.release()
					}
					result <- err
				}()
				e.CloseExecutionSession("scope")
				select {
				case err := <-result:
					if err == nil {
						t.Fatal("close admitted waiting continuation")
					}
				case <-bounded.Done():
					t.Fatal("close/lease deadlock")
				}
				return
			}
			lease, err := e.acquireSessionLease(ctx, "scope", a, url, false)
			if mode == "global-only" {
				if err != nil || lease == nil || lease.session != s {
					t.Fatalf("lost live binding: %v", err)
				}
				lease.release()
			} else {
				if lease != nil {
					lease.release()
				}
				if err == nil {
					t.Fatal("cross-boundary continuation admitted")
				}
			}
		})
	}
}
