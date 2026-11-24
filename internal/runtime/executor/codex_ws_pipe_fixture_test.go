package executor

import (
	"bufio"
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type directFixtureDial func(context.Context, string, string) (net.Conn, error)
type wsPipeHTTPWriter struct {
	conn   net.Conn
	buf    *bufio.ReadWriter
	header http.Header
}

func (w *wsPipeHTTPWriter) Header() http.Header { return w.header }
func (*wsPipeHTTPWriter) Write([]byte) (int, error) {
	return 0, errors.New("unexpected non-upgrade write")
}
func (*wsPipeHTTPWriter) WriteHeader(int)                                {}
func (w *wsPipeHTTPWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.conn, w.buf, nil }

type codexPipeServer struct {
	URL     string
	ctx     context.Context
	cancel  context.CancelFunc
	handler http.Handler
}

func newCodexPipeServer(t *testing.T, handler http.Handler) *codexPipeServer {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return &codexPipeServer{URL: "http://ordinary-codex-fixture.invalid", ctx: ctx, cancel: cancel, handler: handler}
}

func (s *codexPipeServer) Close() { s.cancel() }

func (s *codexPipeServer) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, server := net.Pipe()
	stop := context.AfterFunc(s.ctx, func() { _ = server.Close() })
	go func() {
		defer stop()
		defer server.Close()
		_ = server.SetDeadline(time.Now().Add(4 * time.Second))
		rw := bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server))
		req, err := http.ReadRequest(rw.Reader)
		if err != nil {
			return
		}
		writer := &codexPipeResponse{ResponseRecorder: httptest.NewRecorder(), conn: server, buf: rw}
		s.handler.ServeHTTP(writer, req.WithContext(s.ctx))
		if !writer.hijacked {
			resp := writer.Result()
			defer resp.Body.Close()
			_ = resp.Write(server)
		}
	}()
	return client, nil
}

type codexPipeResponse struct {
	*httptest.ResponseRecorder
	conn     net.Conn
	buf      *bufio.ReadWriter
	hijacked bool
}

func (w *codexPipeResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return w.conn, w.buf, nil
}

func fixtureCodexTenantContext(ctx context.Context) context.Context {
	g, _ := gin.CreateTestContext(nil)
	g.Set("userApiKey", "ordinary-codex-fixture")
	return context.WithValue(ctx, "gin", g)
}
