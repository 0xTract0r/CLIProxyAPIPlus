package executor

import (
	"errors"
	"net/http"
	"testing"

	"github.com/gorilla/websocket"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestOrdinaryWebsocketFrameBudget(t *testing.T) {
	payload := []byte(`{"model":"fixture","input":"content"}`)
	framed := buildCodexWebsocketRequestBody(payload)
	if len(framed) <= len(payload) {
		t.Fatal("fixture did not include framing overhead")
	}
	useHTTP, err := ordinaryCodexWebsocketPreflight(framed, len(framed), false)
	if useHTTP || err != nil {
		t.Fatal("frame at limit was rejected")
	}
	useHTTP, err = ordinaryCodexWebsocketPreflight(framed, len(payload), true)
	if !useHTTP || err != nil {
		t.Fatal("oversize optional WebSocket did not select same-credential HTTP path")
	}
	useHTTP, err = ordinaryCodexWebsocketPreflight(framed, len(payload), false)
	if useHTTP {
		t.Fatal("required WebSocket must not select fallback")
	}
	assertOrdinarySizeIsTerminal(t, err)
}

func TestOrdinaryWebsocketRemote1009DoesNotRetry(t *testing.T) {
	err := mapCodexWebsocketReadError(&websocket.CloseError{Code: websocket.CloseMessageTooBig, Text: "fixture upstream close"})
	assertOrdinarySizeIsTerminal(t, err)
}

func assertOrdinarySizeIsTerminal(t *testing.T, err error) {
	t.Helper()
	var scoped cliproxyexecutor.RequestScopedError
	var status cliproxyexecutor.StatusError
	if !errors.As(err, &scoped) || !scoped.IsRequestScoped() || !errors.As(err, &status) || status.StatusCode() != http.StatusRequestEntityTooLarge || shouldRetryCodexWebsocketSend(err) {
		t.Fatalf("expected request-scoped 413 without retry, got %v", err)
	}
}
