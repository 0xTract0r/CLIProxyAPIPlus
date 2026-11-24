package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executionregistry"
	ce "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type ordinaryHomeDispatch struct {
	homeExecutionDispatcher
	calls int
}

func (d *ordinaryHomeDispatch) RPopAuth(ctx context.Context, p, m string, h http.Header, n int) ([]byte, error) {
	d.calls++
	return d.homeExecutionDispatcher.RPopAuth(ctx, p, m, h, n)
}

type ordinaryHomeDenied struct{ code int }

func (e *ordinaryHomeDenied) Error() string              { return "upstream refused request" }
func (e *ordinaryHomeDenied) StatusCode() int            { return e.code }
func (e *ordinaryHomeDenied) RetryAfter() *time.Duration { d := 61 * time.Second; return &d }

type ordinaryHomeExecutor struct {
	homeExecutionExecutor
	err   error
	calls int
}

func (e *ordinaryHomeExecutor) Execute(context.Context, *Auth, ce.Request, ce.Options) (ce.Response, error) {
	e.calls++
	return ce.Response{}, e.err
}
func (e *ordinaryHomeExecutor) CountTokens(context.Context, *Auth, ce.Request, ce.Options) (ce.Response, error) {
	e.calls++
	return ce.Response{}, e.err
}
func TestHomeReturnsFirstProviderRefusal(t *testing.T) {
	for _, code := range []int{401, 403, 429} {
		for _, count := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d-count-%t", code, count), func(t *testing.T) {
				m := NewManager(nil, nil, nil)
				cfg := &config.Config{Home: config.HomeConfig{Enabled: true}}
				m.SetConfig(cfg)
				m.SetRetryConfig(2, time.Second, 3)
				d := &ordinaryHomeDispatch{}
				m.PublishHomeDispatch(d, executionregistry.New(), 1)
				rejection := &ordinaryHomeDenied{code}
				e := &ordinaryHomeExecutor{err: rejection}
				m.RegisterExecutor(e)
				_, err := m.executeHome(context.Background(), []string{"home-execution"}, ce.Request{Model: "test"}, ce.Options{}, count)
				var received *ordinaryHomeDenied
				if !errors.As(err, &received) || received != rejection || d.calls != 1 || e.calls != 1 {
					t.Fatalf("code=%d dispatch=%d execute=%d err=%v", code, d.calls, e.calls, err)
				}
			})
		}
	}
}
