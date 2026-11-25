package auth

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/ordinaryconnect"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestHTTP500DialBudget(t *testing.T) {
	const provider, model = "offline", "fixture-dial"
	previous := transientErrorCooldownSeconds.Load()
	SetTransientErrorCooldownSeconds(-1)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(previous) })
	for _, tc := range []struct {
		name                    string
		script                  []int
		calls, dials, delivered int
		success                 bool
	}{
		{"refusal_then_500_then_ok", []int{0, 500, 200}, 2, 3, 2, true},
		{"no_new_dial_allowance_on_replay", []int{0, 500, 0, 200}, 2, 3, 1, false},
		{"two_refusals", []int{0, 0, 200}, 1, 2, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			id := "fixture-" + tc.name
			manager := NewManager(nil, &FillFirstSelector{}, nil)
			manager.SetRetryConfig(1, 0, 1)
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { reg.UnregisterClient(id) })
			if _, err := manager.Register(ctx, &Auth{ID: id, Provider: provider}); err != nil {
				t.Fatal(err)
			}
			endpoint, _ := url.Parse("http://fixture.invalid/ordinary")
			calls := 0
			var dials, delivered atomic.Int32
			dial := func(_ context.Context, network, address string) (net.Conn, error) {
				i := int(dials.Add(1)) - 1
				if i >= len(tc.script) || tc.script[i] == 0 {
					return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
				}
				client, peer := net.Pipe()
				go func(status int) {
					defer peer.Close()
					req, err := http.ReadRequest(bufio.NewReader(peer))
					if err != nil {
						return
					}
					_, _ = io.Copy(io.Discard, req.Body)
					req.Body.Close()
					delivered.Add(1)
					_, _ = fmt.Fprintf(peer, "HTTP/1.1 %d %s\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok", status, http.StatusText(status))
				}(tc.script[i])
				return client, nil
			}
			manager.RegisterExecutor(&callbackSchedulerTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: provider},
				execute: func(ctx context.Context, a *Auth, req ex.Request, _ ex.Options) (ex.Response, error) {
					calls++
					if a.ID != id || req.Model != model {
						t.Fatal("changed target")
					}
					bound := ordinaryconnect.Bind(ctx, id, endpoint)
					tr := &http.Transport{DialContext: dial}
					ordinaryconnect.Wrap(bound, tr)
					defer tr.CloseIdleConnections()
					r, _ := http.NewRequestWithContext(bound, "POST", endpoint.String(), nil)
					resp, err := tr.RoundTrip(r)
					if err != nil {
						return ex.Response{}, err
					}
					defer resp.Body.Close()
					ex.MarkUpstreamAttempt(ctx)
					payload, err := io.ReadAll(resp.Body)
					if err != nil {
						return ex.Response{}, err
					}
					if resp.StatusCode != 200 {
						return ex.Response{}, &Error{HTTPStatus: resp.StatusCode, Message: "synthetic HTTP response"}
					}
					return ex.Response{Payload: payload}, nil
				},
			})
			res, err := manager.Execute(ctx, []string{provider}, ex.Request{Model: model}, ex.Options{})
			t.Logf("executor=%d dials=%d delivered_HTTP=%d success=%t", calls, dials.Load(), delivered.Load(), err == nil)
			if (err == nil) != tc.success || (tc.success && string(res.Payload) != "ok") || calls != tc.calls || int(dials.Load()) != tc.dials || int(delivered.Load()) != tc.delivered {
				t.Fatal("unexpected budget or response")
			}
		})
	}
}
