package executor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translation "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryAntigravityReusesStandardTransportAcrossSDKOperations(t *testing.T) {
	var dials, calls atomic.Int32
	transport := &http.Transport{DialTLSContext: func(_ context.Context, _, address string) (net.Conn, error) {
		if address != "fixture.invalid:443" {
			return nil, fmt.Errorf("unexpected destination %s", address)
		}
		dials.Add(1)
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			reader := bufio.NewReader(server)
			for {
				req, err := http.ReadRequest(reader)
				if err != nil {
					return
				}
				if req.Body != nil {
					_, _ = io.Copy(io.Discard, req.Body)
					_ = req.Body.Close()
				}
				calls.Add(1)
				if req.Header.Get("Authorization") != "Bearer fixture" {
					_, _ = io.WriteString(server, "HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n")
					return
				}
				mediaType := "application/json"
				body := `{"totalTokens":3}`
				switch req.URL.Path {
				case antigravityStreamPath:
					mediaType = "text/event-stream"
					body = "data: " + `{"response":{"candidates":[{"content":{"parts":[{"text":"fixture"}]},"finishReason":"STOP"}]}}` + "\n\n"
				case "/health":
					body = `{"ready":true}`
				case antigravityCountTokensPath:
				default:
					_, _ = io.WriteString(server, "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n")
					return
				}
				if _, err := fmt.Fprintf(server, "HTTP/1.1 200 OK\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n%s", mediaType, len(body), body); err != nil {
					return
				}
			}
		}()
		return client, nil
	}}
	t.Cleanup(transport.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", transport)
	e := NewAntigravityExecutor(&config.Config{})
	auth := ordinaryPortAuth()
	req := executorapi.Request{Model: "fixture-model", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)}
	opts := executorapi.Options{SourceFormat: translation.FormatGemini, ResponseFormat: translation.FormatAntigravity}
	if _, err := e.CountTokens(ctx, auth, req, opts); err != nil {
		t.Fatal(err)
	}
	stream, err := e.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	for event := range stream.Chunks {
		if event.Err != nil {
			t.Fatal(event.Err)
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://fixture.invalid/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.HttpRequest(ctx, auth, httpReq)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil || !strings.Contains(string(raw), "ready") || dials.Load() != 1 || calls.Load() != 3 {
		t.Fatalf("standard transport reuse changed: dials=%d calls=%d read=%v", dials.Load(), calls.Load(), err)
	}
}
