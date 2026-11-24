package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"syscall"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/ordinaryconnect"
	ce "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Single ordinary grant, two model candidates. The executor uses the production
// dial wrapper around the same injected callback; no socket/listener is opened.
type ordinaryTwoModelDialExecutor struct {
	*openAICompatPoolExecutor
	grant   string
	models  []string
	dials   int
	refusal error
}

func (e *ordinaryTwoModelDialExecutor) Execute(ctx context.Context, a *Auth, req ce.Request, _ ce.Options) (ce.Response, error) {
	if a == nil || a.ID != e.grant {
		return ce.Response{}, errors.New("unexpected ordinary grant")
	}
	e.models = append(e.models, req.Model)
	u, _ := url.Parse("http://127.0.0.1:18419/v1/chat/completions")
	bound := ordinaryconnect.Bind(ctx, a.ID, u)
	var peer net.Conn
	transport := &http.Transport{DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
		e.dials++
		if network != "tcp" || address != "127.0.0.1:18419" {
			return nil, errors.New("ordinary route changed")
		}
		if req.Model == "ordinary-upstream-first" {
			return nil, e.refusal
		}
		first, second := net.Pipe()
		peer = second
		return first, nil
	}}
	ordinaryconnect.Wrap(bound, transport)
	conn, err := transport.DialContext(bound, "tcp", "127.0.0.1:18419")
	if conn != nil {
		_ = conn.Close()
	}
	if peer != nil {
		_ = peer.Close()
	}
	if err != nil {
		return ce.Response{}, err
	}
	return ce.Response{Payload: []byte("ordinary second model success")}, nil
}
func TestOrdinaryOneGrantModelPoolConnectBudget(t *testing.T) {
	for _, budget := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			alias := "ordinary-two-model-budget-" + fmt.Sprint(budget)
			base := &openAICompatPoolExecutor{id: openAICompatPoolProviderKey}
			m := newOpenAICompatPoolTestManager(t, alias, []internalconfig.OpenAICompatibilityModel{
				{Name: "ordinary-upstream-first", Alias: alias}, {Name: "ordinary-upstream-second", Alias: alias},
			}, base)
			m.SetRetryConfig(budget, 0, 0)
			entries := m.List()
			if len(entries) != 1 {
				t.Fatal("fixture must have exactly one grant")
			}
			grant := entries[0]
			grant.ProxyURL = "direct"
			if _, err := m.Update(context.Background(), grant); err != nil {
				t.Fatal("ordinary direct fixture")
			}
			refusal := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
			exec := &ordinaryTwoModelDialExecutor{openAICompatPoolExecutor: base, grant: grant.ID, refusal: refusal}
			m.RegisterExecutor(exec)
			response, err := m.Execute(context.Background(), []string{openAICompatPoolProviderKey}, ce.Request{Model: alias}, ce.Options{})
			if budget == 0 {
				if err != nil || string(response.Payload) != "ordinary second model success" || exec.dials != 2 || !reflect.DeepEqual(exec.models, []string{"ordinary-upstream-first", "ordinary-upstream-second"}) {
					t.Fatalf("budget0 fallback changed: err=%v dials=%d models=%v", err, exec.dials, exec.models)
				}
			} else {
				var got *net.OpError
				if !errors.Is(err, syscall.ECONNREFUSED) || !errors.As(err, &got) || got != refusal || len(response.Payload) != 0 || exec.dials != budget+1 || !reflect.DeepEqual(exec.models, []string{"ordinary-upstream-first"}) {
					t.Fatalf("budget %d leaked fallback: err=%v dials=%d models=%v", budget, err, exec.dials, exec.models)
				}
			}
			if len(m.List()) != 1 {
				t.Fatal("single-grant fixture changed")
			}
		})
	}
}
