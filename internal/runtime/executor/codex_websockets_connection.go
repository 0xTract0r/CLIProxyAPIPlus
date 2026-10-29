package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
	"golang.org/x/net/proxy"
)

const (
	codexResponsesWebsocketBetaHeaderValue = "responses_websockets=2026-02-06"
	codexResponsesWebsocketIdleTimeout     = 5 * time.Minute
	codexResponsesWebsocketPingInterval    = 30 * time.Second
	codexResponsesWebsocketHandshakeTO     = 30 * time.Second
)

func (e *CodexWebsocketsExecutor) dialCodexWebsocket(ctx context.Context, auth *cliproxyauth.Auth, wsURL string, headers http.Header) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error) {
	dialer := newProxyAwareWebsocketDialer(e.cfg, auth)
	dialer.HandshakeTimeout = codexResponsesWebsocketHandshakeTO
	dialer.EnableCompression = true
	// Present the real codex-rs (rustls) uTLS ClientHello on the wss handshake
	// instead of a bare Go TLS ClientHello. This is codex-only: the shared
	// newProxyAwareWebsocketDialer is also used by the xAI executor, which must
	// keep its own TLS stack, so the codex-rs fingerprint is applied here rather
	// than inside the shared dialer.
	applyCodexRustlsWebsocketTLS(dialer, codexWebsocketEffectiveProxyURL(e.cfg, auth))
	if ctx == nil {
		ctx = context.Background()
	}
	// Gorilla does not interrupt the HTTP upgrade read on cancellation after
	// dialing TCP. Keep ownership of that socket only until the handshake ends.
	var dialMu sync.Mutex
	var handshakeConn net.Conn
	handshakeFinished := false
	wrapDial := func(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
		return func(dialCtx context.Context, network, address string) (net.Conn, error) {
			rawConn, errDial := dial(dialCtx, network, address)
			if errDial != nil {
				return nil, errDial
			}
			dialMu.Lock()
			if ctx.Err() != nil {
				dialMu.Unlock()
				_ = rawConn.Close()
				return nil, ctx.Err()
			}
			handshakeConn = rawConn
			dialMu.Unlock()
			return rawConn, nil
		}
	}
	netDial := dialer.NetDialContext
	if netDial == nil {
		netDial = (&net.Dialer{}).DialContext
	}
	dialer.NetDialContext = wrapDial(netDial)
	if dialer.NetDialTLSContext != nil {
		dialer.NetDialTLSContext = wrapDial(dialer.NetDialTLSContext)
	}
	stopCancel := context.AfterFunc(ctx, func() {
		dialMu.Lock()
		defer dialMu.Unlock()
		if !handshakeFinished && handshakeConn != nil {
			_ = handshakeConn.Close()
		}
	})
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	stopCancel()
	dialMu.Lock()
	handshakeFinished = true
	dialMu.Unlock()
	if ctx.Err() != nil {
		if conn != nil {
			_ = conn.Close()
			conn = nil
		}
		helps.LogWithRequestID(ctx).Info("codex websocket setup outcome=context_done")
		return nil, nil, resp, ctx.Err()
	}
	if err != nil {
		failureClass := codexWebsocketSetupFailureClass(err)
		if resp != nil {
			helps.LogWithRequestID(ctx).Infof("codex websocket setup outcome=upstream_rejection status=%d", resp.StatusCode)
		} else if failureClass != "" {
			// No WebSocket has been established and no response.create was sent.
			// Reuse transient credential scheduling after the existing uTLS retry
			// budget, without adding retries or replaying an active generation.
			helps.LogWithRequestID(ctx).Infof("codex websocket setup outcome=transport_error class=%s status=503", failureClass)
			err = codexWebsocketSetupError{cause: err}
		} else {
			helps.LogWithRequestID(ctx).Info("codex websocket setup outcome=failed class=other")
		}
	}
	closer := newWebsocketConnectionCloser(conn)
	if conn != nil {
		// Avoid gorilla/websocket flate tail validation issues on some upstreams/Go versions.
		// Negotiating permessage-deflate is fine; we just don't compress outbound messages.
		conn.EnableWriteCompression(false)
		conn.SetPingHandler(func(appData string) error {
			if err := conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout)); err != nil {
				return err
			}
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
		})
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		})
		startCodexWebsocketHeartbeat(closer, codexResponsesWebsocketPingInterval)
	}
	return conn, closer, resp, err
}

type codexWebsocketSetupError struct{ cause error }

func (codexWebsocketSetupError) Error() string {
	return `{"error":{"message":"upstream websocket connection temporarily unavailable","type":"server_error","code":"upstream_connection_error"}}`
}
func (e codexWebsocketSetupError) Unwrap() error { return e.cause }
func (codexWebsocketSetupError) StatusCode() int { return http.StatusServiceUnavailable }

func codexWebsocketSetupFailureClass(err error) string {
	if err == nil || errors.Is(err, context.Canceled) {
		return ""
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		if strings.Contains(strings.ToLower(err.Error()), "socks") {
			return "proxy_connect_eof"
		}
		return "eof"
	}
	var networkError net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout() {
		return "timeout"
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return "connection_reset"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection_refused"
	}
	return ""
}

// Control writes may run alongside the single data writer. Closing the owning
// resource stops the heartbeat; sending a ping alone never refreshes liveness.
func startCodexWebsocketHeartbeat(closer *websocketConnectionCloser, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-closer.done:
				return
			case <-ticker.C:
				if err := closer.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					_ = closer.Close()
					return
				}
			}
		}
	}()
	return done
}

func writeCodexWebsocketMessage(sess *codexWebsocketSession, conn *websocket.Conn, payload []byte) error {
	if sess != nil {
		return sess.writeMessage(conn, websocket.TextMessage, payload)
	}
	if conn == nil {
		return fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func mapCodexWebsocketWriteError(sess *codexWebsocketSession, conn *websocket.Conn, err error) error {
	if err == nil || sess == nil || conn == nil {
		return err
	}
	upstreamErr := sess.upstreamDisconnectError(conn)
	var closeErr *websocket.CloseError
	if !errors.As(upstreamErr, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig {
		return err
	}
	return mapCodexWebsocketReadError(upstreamErr)
}

func shouldRetryCodexWebsocketSend(err error) bool {
	if err == nil {
		return false
	}
	var requestErr cliproxyexecutor.RequestScopedError
	return !errors.As(err, &requestErr) || !requestErr.IsRequestScoped()
}

type codexWebsocketMessageTooBigError struct {
	statusErr
}

func (codexWebsocketMessageTooBigError) IsRequestScoped() bool {
	return true
}

func isCodexWebsocketMessageTooBig(err error) bool {
	var tooBig codexWebsocketMessageTooBigError
	return errors.As(err, &tooBig)
}

func mapCodexWebsocketReadError(err error) error {
	if err == nil {
		return nil
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) && closeErr.Code == websocket.CloseMessageTooBig {
		return codexWebsocketMessageTooBigError{statusErr: statusErr{
			code: http.StatusRequestEntityTooLarge,
			msg:  `{"error":{"message":"upstream websocket message too big","type":"invalid_request_error","code":"message_too_big"}}`,
		}}
	}
	return err
}

func normalizeCodexWebsocketParallelToolCalls(body []byte, headers http.Header) []byte {
	if !isCodexResponsesLiteRequest(body, headers) {
		return body
	}
	body = helps.SetBoolIfDifferent(body, "parallel_tool_calls", false)
	return body
}

func buildCodexWebsocketRequestBody(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}

	// Match codex-rs websocket v2 semantics: every request is `response.create`.
	// Incremental follow-up turns continue on the same websocket using
	// `previous_response_id` + incremental `input`, not `response.append`.
	body = helps.SanitizeCodexInputItemIDs(body)
	wsReqBody, errSet := sjson.SetBytes(bytes.Clone(body), "type", "response.create")
	if errSet == nil && len(wsReqBody) > 0 {
		return wsReqBody
	}
	fallback := bytes.Clone(body)
	fallback, _ = sjson.SetBytes(fallback, "type", "response.create")
	return fallback
}

func readCodexWebsocketMessage(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn, readCh chan codexWebsocketRead) (int, []byte, error) {
	event := readCodexWebsocketEvent(ctx, sess, conn, readCh)
	return event.msgType, event.payload, event.err
}

func readCodexWebsocketEvent(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn, readCh chan codexWebsocketRead) codexWebsocketRead {
	if sess == nil {
		if conn == nil {
			return codexWebsocketRead{err: fmt.Errorf("codex websockets executor: websocket conn is nil")}
		}
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		// This connection belongs to one request. Interrupt ReadMessage when
		// its downstream cancels instead of waiting for the liveness deadline.
		if ctx == nil {
			ctx = context.Background()
		}
		stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stopCancel()
		started := time.Now()
		msgType, payload, errRead := conn.ReadMessage()
		arrival := time.Now()
		if ctx.Err() != nil {
			errRead = ctx.Err()
		}
		return codexWebsocketRead{conn: conn, msgType: msgType, payload: payload, err: errRead, arrival: arrival, readDuration: arrival.Sub(started)}
	}
	if conn == nil {
		return codexWebsocketRead{err: fmt.Errorf("codex websockets executor: websocket conn is nil")}
	}
	if readCh == nil {
		return codexWebsocketRead{err: fmt.Errorf("codex websockets executor: session read channel is nil")}
	}
	for {
		select {
		case <-ctx.Done():
			return codexWebsocketRead{err: ctx.Err()}
		case ev, ok := <-readCh:
			if !ok {
				return codexWebsocketRead{err: fmt.Errorf("codex websockets executor: session read channel closed")}
			}
			if ev.conn != conn {
				continue
			}
			return ev
		}
	}
}

func newProxyAwareWebsocketDialer(cfg *config.Config, auth *cliproxyauth.Auth) *websocket.Dialer {
	dialer := &websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  codexResponsesWebsocketHandshakeTO,
		EnableCompression: true,
		NetDialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	proxyURL := ""
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	if proxyURL == "" {
		return dialer
	}

	setting, errParse := proxyutil.Parse(proxyURL)
	if errParse != nil {
		log.Errorf("codex websockets executor: %v", errParse)
		return dialer
	}

	switch setting.Mode {
	case proxyutil.ModeDirect:
		dialer.Proxy = nil
		return dialer
	case proxyutil.ModeProxy:
	default:
		return dialer
	}

	switch setting.URL.Scheme {
	case "socks5", "socks5h":
		var proxyAuth *proxy.Auth
		if setting.URL.User != nil {
			username := setting.URL.User.Username()
			password, _ := setting.URL.User.Password()
			proxyAuth = &proxy.Auth{User: username, Password: password}
		}
		socksDialer, errSOCKS5 := proxy.SOCKS5("tcp", setting.URL.Host, proxyAuth, proxy.Direct)
		if errSOCKS5 != nil {
			log.Errorf("codex websockets executor: create SOCKS5 dialer failed: %v", errSOCKS5)
			return dialer
		}
		dialer.Proxy = nil
		dialer.NetDialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
			return socksDialer.Dial(network, addr)
		}
	case "http", "https":
		dialer.Proxy = http.ProxyURL(setting.URL)
	default:
		log.Errorf("codex websockets executor: unsupported proxy scheme: %s", setting.URL.Scheme)
	}

	return dialer
}

// applyCodexRustlsWebsocketTLS routes the codex responses WebSocket wss handshake
// through the strict codex-rs (rustls) uTLS dialer so the outbound ClientHello is
// the real codex-rs fingerprint (JA3 e4d448cd...) rather than a bare Go TLS
// ClientHello (JA3 03117a8e...). It is codex-only and must not be applied to the
// xAI executor, which shares newProxyAwareWebsocketDialer but keeps its own TLS.
//
// dialer.Proxy is forced to nil: gorilla/websocket still consults Dialer.Proxy
// even when NetDialTLSContext is set and would otherwise wrap the uTLS dialer as
// the base dialer used to REACH a proxy (double-proxy). With Proxy nil the uTLS
// closure is the sole owner of the proxy tunnel (which it already performs via
// proxyutil.BuildDialer for the same proxyURL). NetDialContext, set by
// newProxyAwareWebsocketDialer for the plaintext ws:// path, is left intact; codex
// is always wss so it is not exercised in production.
//
// Fail-closed: if the codex-rs uTLS dialer cannot be constructed, every dial fails
// with the construction error instead of downgrading to bare Go TLS or plaintext.
func applyCodexRustlsWebsocketTLS(dialer *websocket.Dialer, proxyURL string) {
	if dialer == nil {
		return
	}
	dialer.Proxy = nil
	tlsDial, errTLS := helps.NewCodexRustlsTLSDialer(strings.TrimSpace(proxyURL))
	if errTLS != nil {
		log.Errorf("codex websockets executor: build codex-rs uTLS dialer failed: %v", errTLS)
		dialer.NetDialTLSContext = func(context.Context, string, string) (net.Conn, error) {
			return nil, fmt.Errorf("codex websockets executor: codex-rs uTLS dialer unavailable: %w", errTLS)
		}
		return
	}
	dialer.NetDialTLSContext = tlsDial
}

func buildCodexResponsesWebsocketURL(httpURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(httpURL))
	if err != nil {
		return "", err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("codex websockets executor: unsupported responses websocket URL scheme %q", parsed.Scheme)
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("codex websockets executor: responses websocket URL host is empty")
	}
	return parsed.String(), nil
}
