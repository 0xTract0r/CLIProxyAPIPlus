package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const ampInboundFixture = "synthetic-amp-inbound"

func newAmpForwardingFixture(t *testing.T, settings config.AmpCode) *Server {
	t.Helper()
	cfg := &config.Config{AmpCode: settings}
	cfg.APIKeys = []string{ampInboundFixture}
	return newTestServerWithConfig(t, cfg, WithIsolatedUsage())
}

func attachAmpPipe(t *testing.T, s *Server, host string, serve func(net.Conn)) {
	t.Helper()
	state := s.ampForward.Load()
	if state == nil || state.proxy == nil {
		t.Fatal("forwarding proxy was not published")
	}
	transport := ampForwardTransport.Clone()
	transport.DialContext = func(_ context.Context, _, target string) (net.Conn, error) {
		if target != host+":80" {
			return nil, fmt.Errorf("unexpected fixture destination: %s", target)
		}
		local, remote := net.Pipe()
		go func() {
			defer remote.Close()
			serve(remote)
		}()
		return local, nil
	}
	state.proxy.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
}

func ampFixtureRequest(method, path, body, remote, key string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = remote
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	return r
}

func TestOrdinaryAmpForwardingAndReload(t *testing.T) {
	const firstHost = "amp-first.example.invalid"
	const secondHost = "amp-second.example.invalid"
	s := newAmpForwardingFixture(t, config.AmpCode{
		UpstreamURL: "http://" + firstHost + "/fixture-base", UpstreamAPIKey: "synthetic-service-first",
		RestrictManagementToLocalhost: true,
	})
	type observation struct {
		host, method, path, query, auth, apiKey, body, cookie, forwarded string
	}
	seen := make(chan observation, 16)
	respond := func(conn net.Conn) {
		r, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			return
		}
		seen <- observation{
			r.Host, r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"),
			r.Header.Get("X-Api-Key"), string(body), r.Header.Get("Cookie"), r.Header.Get("X-Forwarded-For"),
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 202 Accepted\r\nContent-Length: 2\r\nConnection: close\r\n"+
			"Set-Cookie: private-fixture=1\r\nAccess-Control-Allow-Origin: *\r\nX-Amp-Fixture: response\r\n\r\nok")
	}
	attachAmpPipe(t, s, firstHost, respond)
	call := func(method, path, body, remote, key string) *httptest.ResponseRecorder {
		r := ampFixtureRequest(method, path, body, remote, key)
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		r.Header.Set("X-Real-IP", "127.0.0.1")
		r.Header.Set("Cookie", "private-client-fixture=1")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	for _, c := range []struct {
		method, path, remote, key string
		status                    int
	}{
		{http.MethodGet, "/api/user/profile", "192.0.2.10:5300", ampInboundFixture, 403},
		{http.MethodGet, "/api/user", "127.0.0.1:5300", "", 401},
		{http.MethodGet, "/api/user", "127.0.0.1:5300", "wrong-synthetic-key", 401},
		{http.MethodGet, "/docs", "127.0.0.1:5300", "", 401},
		{http.MethodOptions, "/api/user", "127.0.0.1:5300", "", 204},
	} {
		w := call(c.method, c.path, "", c.remote, c.key)
		if w.Code != c.status {
			t.Fatalf("%s %s returned %d want %d", c.method, c.path, w.Code, c.status)
		}
		if c.status != 404 && w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("Amp response permits cross-origin reads: %s", c.path)
		}
	}
	if len(seen) != 0 {
		t.Fatal("refused requests reached the fixture")
	}
	assertForward := func(method, path, remote, host, credential, wantPath string) {
		t.Helper()
		w := call(method, path+"?page=2&key=private-query&auth_token=private-query", "incoming", remote, ampInboundFixture)
		if w.Code != http.StatusAccepted || w.Body.String() != "ok" || w.Header().Get("Set-Cookie") != "" ||
			w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("X-Amp-Fixture") != "response" {
			t.Fatalf("unexpected proxy response code=%d body=%q", w.Code, w.Body)
		}
		got := <-seen
		if got.host != host || got.path != wantPath || got.method != method || got.auth != "Bearer "+credential ||
			got.apiKey != credential || got.query != "page=2" || got.body != "incoming" || got.cookie != "" || got.forwarded != "" {
			t.Fatalf("unexpected proxy request host=%q method=%q path=%q", got.host, got.method, got.path)
		}
	}
	assertForward(http.MethodPost, "/api/threads/1", "127.0.0.1:5300", firstHost, "synthetic-service-first", "/fixture-base/api/threads/1")
	assertForward(http.MethodGet, "/docs/start", "[::1]:5300", firstHost, "synthetic-service-first", "/fixture-base/docs/start")
	cfg := s.getConfig().CloneForRuntime()
	cfg.AmpCode.UpstreamURL = "https://user:password@amp-invalid.example.invalid/"
	cfg.AmpCode.UpstreamAPIKey = "synthetic-rejected"
	cfg.AmpCode.RestrictManagementToLocalhost = false
	if !s.UpdateClientsContext(context.Background(), cfg) {
		t.Fatal("ordinary configuration reload failed")
	}
	if w := call(http.MethodGet, "/api/user", "", "192.0.2.10:5300", ampInboundFixture); w.Code != 403 {
		t.Fatal("invalid upstream reload loosened localhost restriction")
	}
	assertForward(http.MethodPost, "/api/meta", "127.0.0.1:5300", firstHost, "synthetic-service-first", "/fixture-base/api/meta")
	cfg = cfg.CloneForRuntime()
	cfg.AmpCode.UpstreamURL = "http://" + secondHost
	cfg.AmpCode.UpstreamAPIKey = "synthetic-service-second"
	if !s.UpdateClientsContext(context.Background(), cfg) {
		t.Fatal("updated upstream was not accepted")
	}
	attachAmpPipe(t, s, secondHost, respond)
	assertForward(http.MethodPost, "/api/tab", "192.0.2.10:5300", secondHost, "synthetic-service-second", "/api/tab")
	cfg = cfg.CloneForRuntime()
	cfg.AmpCode.UpstreamURL = ""
	if !s.UpdateClientsContext(context.Background(), cfg) {
		t.Fatal("disabling upstream failed")
	}
	if w := call(http.MethodGet, "/api/user", "", "127.0.0.1:5300", ampInboundFixture); w.Code != 503 {
		t.Fatalf("disabled upstream returned %d", w.Code)
	}
}

type ampStreamRecorder struct {
	*httptest.ResponseRecorder
	once     sync.Once
	hasChunk atomic.Bool
	flushed  chan struct{}
}

func (r *ampStreamRecorder) Write(payload []byte) (int, error) {
	n, err := r.ResponseRecorder.Write(payload)
	if n > 0 {
		r.hasChunk.Store(true)
	}
	return n, err
}

func (r *ampStreamRecorder) Flush() {
	r.ResponseRecorder.Flush()
	if r.hasChunk.Load() {
		r.once.Do(func() { close(r.flushed) })
	}
}

func TestOrdinaryAmpForwardStreamingCancellation(t *testing.T) {
	const host = "amp-stream.example.invalid"
	s := newAmpForwardingFixture(t, config.AmpCode{
		UpstreamURL: "http://" + host, UpstreamAPIKey: "synthetic-stream-service",
		RestrictManagementToLocalhost: true,
	})
	cancelled := make(chan error, 1)
	attachAmpPipe(t, s, host, func(conn net.Conn) {
		r, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			cancelled <- err
			return
		}
		_ = r.Body.Close()
		const event = "data: ordinary\n\n"
		_, err = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(event), event)
		if err == nil {
			_, err = conn.Read(make([]byte, 1))
		}
		cancelled <- err
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := ampFixtureRequest(http.MethodGet, "/api/threads/events", "", "127.0.0.1:5301", ampInboundFixture).WithContext(ctx)
	w := &ampStreamRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		s.Handler().ServeHTTP(w, r)
		close(done)
	}()
	select {
	case <-w.flushed:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream SSE header/chunk was not flushed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("forwarding did not stop on downstream cancellation")
	}
	select {
	case err := <-cancelled:
		if err != io.EOF && err != io.ErrClosedPipe {
			t.Fatalf("unexpected local upstream read error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not close the upstream socket")
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "data: ordinary") {
		t.Fatalf("streamed fixture was incomplete: code=%d body=%q", w.Code, w.Body)
	}
}
