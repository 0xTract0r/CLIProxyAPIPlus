package executor

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexWebsocketSetupProxyEOFCoolsWithoutExtraDialRetries(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3; i++ {
			conn, errAccept := listener.Accept()
			if errAccept != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			_, _ = io.ReadFull(conn, make([]byte, 3))
			attempts.Add(1)
			_ = conn.Close() // EOF during SOCKS negotiation, before any WS request.
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })

	exec := newCodexLeaseTestExecutor()
	auth := &cliproxyauth.Auth{ProxyURL: "socks5h://" + listener.Addr().String()}
	conn, closer, resp, errDial := exec.dialCodexWebsocket(context.Background(), auth, "wss://example.invalid/responses", nil)
	if conn != nil || closer != nil || resp != nil || errDial == nil {
		t.Fatalf("expected setup failure without a response: conn=%v closer=%v resp=%v err=%v", conn, closer, resp, errDial)
	}
	var status interface{ StatusCode() int }
	if !errors.As(errDial, &status) || status.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("setup EOF must carry transient status 503: %v", errDial)
	}
	if !errors.Is(errDial, io.EOF) {
		t.Fatalf("setup failure lost its EOF cause: %v", errDial)
	}
	if strings.Contains(errDial.Error(), listener.Addr().String()) {
		t.Fatal("public setup error exposed the proxy address")
	}
	if attempts.Load() != 3 {
		t.Fatalf("dial attempts=%d, want existing uTLS budget of 3", attempts.Load())
	}
}

func TestCodexWebsocketSetupPreservesCancellationAndHTTPRejection(t *testing.T) {
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		exec := newCodexLeaseTestExecutor()
		_, _, _, err := exec.dialCodexWebsocket(ctx, &cliproxyauth.Auth{ProxyURL: "direct"}, "ws://127.0.0.1:1/responses", nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller cancellation changed: %v", err)
		}
		var status interface{ StatusCode() int }
		if errors.As(err, &status) {
			t.Fatalf("caller cancellation must not cool the credential: %v", err)
		}
	})
	t.Run("http_rejection", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "invalid request", http.StatusBadRequest)
		}))
		defer server.Close()
		exec := newCodexLeaseTestExecutor()
		url, _ := buildCodexResponsesWebsocketURL(server.URL)
		_, _, resp, err := exec.dialCodexWebsocket(context.Background(), &cliproxyauth.Auth{ProxyURL: "direct"}, url, nil)
		if resp == nil || resp.StatusCode != http.StatusBadRequest || err == nil {
			t.Fatalf("handshake rejection changed: resp=%v err=%v", resp, err)
		}
		defer func() { _ = resp.Body.Close() }()
		var status interface{ StatusCode() int }
		if errors.As(err, &status) {
			t.Fatalf("explicit upstream rejection was replaced: %v", err)
		}
	})
}
