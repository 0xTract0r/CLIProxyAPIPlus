package auth_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type beforeMarkGate struct {
	cliproxyexecutor.HTTPAttemptGate
	beforeMark func()
}
type beforeMarkPermit struct {
	cliproxyexecutor.HTTPAttemptPermit
	beforeMark func()
}

func (g beforeMarkGate) Before(ctx context.Context, info cliproxyexecutor.HTTPAttemptInfo) (cliproxyexecutor.HTTPAttemptPermit, error) {
	permit, err := g.HTTPAttemptGate.Before(ctx, info)
	if err != nil {
		return nil, err
	}
	return beforeMarkPermit{permit, g.beforeMark}, nil
}
func (p beforeMarkPermit) MarkSent(ctx context.Context) error {
	p.beforeMark()
	return p.HTTPAttemptPermit.MarkSent(ctx)
}

func TestWarmupPacingHTTPHelperPoolReselect(t *testing.T) {
	coreauth.CheckWarmupPacingHTTPPoolReselect(t, func(ctx context.Context, req *http.Request, beforeMark func()) (func(error) error, error) {
		gate := cliproxyexecutor.HTTPAttemptGateFromContext(ctx)
		if gate == nil {
			t.Fatal("missing real Manager gate")
		}
		ctx = cliproxyexecutor.WithHTTPAttemptGate(ctx, beforeMarkGate{gate, beforeMark})
		attempt, err := helps.BeginClaudeHTTPAttempt(ctx, req.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		return attempt.TransportFailed, nil
	})
}
