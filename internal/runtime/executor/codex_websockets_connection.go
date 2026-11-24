package executor

import (
	"compress/flate"
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

	"github.com/MoeForge/moego/ojson"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

const (
	codexResponsesWebsocketBetaHeaderValue = "responses_websockets=2026-02-06"
	codexResponsesWebsocketIdleTimeout     = 5 * time.Minute
	codexResponsesWebsocketPingInterval    = 30 * time.Second
	codexResponsesWebsocketHandshakeTO     = 30 * time.Second
)

func (e *CodexWebsocketsExecutor) dialCodexWebsocket(ctx context.Context, auth *cliproxyauth.Auth, wsURL string, headers http.Header) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error) {
	dialer := newProxyAwareWebsocketDialer(ctx, e.cfg, auth)
	if e.directDialContext != nil {
		route, err := proxyutil.Parse(executionProxyURL(ctx, e.cfg, auth))
		if err == nil && route.Mode == proxyutil.ModeDirect {
			dialer.NetDialContext = e.directDialContext
		}
	}
	dialer.HandshakeTimeout = codexResponsesWebsocketHandshakeTO
	dialer.EnableCompression = true
	if ctx == nil {
		ctx = context.Background()
	}
	conn, resp, err := dialWebsocketWithUpgradeCancellation(ctx, dialer, wsURL, headers)
	if ctxErr := websocketUpgradeContextError(ctx, err); ctxErr != nil && errors.Is(err, ctxErr) {
		helps.LogWithRequestID(ctx).Info("codex websocket setup outcome=context_done")
		return nil, nil, resp, err
	}

	if err != nil {
		cliproxyexecutor.MarkUpstreamAttempt(ctx)
		failureClass := codexWebsocketSetupFailureClass(err)
		if resp != nil {
			helps.LogWithRequestID(ctx).Infof("codex websocket setup outcome=upstream_rejection status=%d", resp.StatusCode)
		} else if failureClass != "" {
			// No WebSocket has been established and no response.create was sent.
			// Reuse transient credential scheduling without adding retries or replaying an active generation.
			helps.LogWithRequestID(ctx).Infof("codex websocket setup outcome=transport_error class=%s status=503", failureClass)
			err = codexWebsocketSetupError{cause: err}
		} else {
			helps.LogWithRequestID(ctx).Info("codex websocket setup outcome=failed class=other")
		}
	}
	closer := newWebsocketConnectionCloser(conn)
	if conn != nil {
		enabled := e.cfg == nil || e.cfg.Codex.WebsocketCompressionEnabled()
		conn.EnableWriteCompression(enabled)
		if enabled {
			// DefaultCompression avoids the fast flate tail path; configuration remains an escape hatch.
			if errSetCompressionLevel := conn.SetCompressionLevel(flate.DefaultCompression); errSetCompressionLevel != nil {
				log.Errorf("set Codex websocket compression level: %v", errSetCompressionLevel)
			}
		}
		conn.SetPingHandler(func(appData string) error {
			closer.liveness.ObserveControl("ping_received")
			if err := conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout)); err != nil {
				return err
			}
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
		})
		conn.SetPongHandler(func(string) error {
			closer.liveness.ObserveControl("pong_received")
			return conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		})
		startCodexWebsocketHeartbeat(closer, codexResponsesWebsocketPingInterval)
	}
	return conn, closer, resp, err
}

func dialWebsocketWithUpgradeCancellation(ctx context.Context, dialer *websocket.Dialer, wsURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	private := *dialer
	dialer = &private
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
	dialMu.Lock()
	handshakeFinished = true
	dialMu.Unlock()
	stopCancel()
	if ctxErr := websocketUpgradeContextError(ctx, err); ctxErr != nil && (err == nil || resp == nil) {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, resp, ctxErr
	}
	return conn, resp, err
}

// The socket deadline and the context timer can expire in either order.
func websocketUpgradeContextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		var networkErr net.Error
		if errors.As(err, &networkErr) && networkErr.Timeout() {
			return context.DeadlineExceeded
		}
	}
	return nil
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
					closer.liveness.ObserveControl("ping_write_error")
					_ = closer.Close()
					return
				}
				closer.liveness.ObserveControl("ping_sent")
			}
		}
	}()
	return done
}

func writeWebsocketPayloadMessage(provider string, sess *codexWebsocketSession, conn *websocket.Conn, payload []byte) error {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "codex"
	}
	sessionID := ""
	if sess != nil {
		sessionID = sess.sessionID
	}
	sessionKind := sessionObjectKind(sess)
	payloadBytes := len(payload)
	start := time.Now()
	log.Debugf("%s websockets: write payload started session=%s session_object=%s bytes=%d", provider, sessionID, sessionKind, payloadBytes)
	var errSend error
	if sess != nil {
		errSend = sess.writeMessage(conn, websocket.TextMessage, payload)
	} else if conn == nil {
		errSend = fmt.Errorf("%s websockets executor: websocket conn is nil", provider)
	} else {
		errSend = conn.WriteMessage(websocket.TextMessage, payload)
	}
	if errSend != nil {
		log.Warnf("%s websockets: write payload failed session=%s session_object=%s bytes=%d duration=%v err=%v", provider, sessionID, sessionKind, payloadBytes, time.Since(start), errSend)
	} else {
		log.Debugf("%s websockets: write payload completed session=%s session_object=%s bytes=%d duration=%v", provider, sessionID, sessionKind, payloadBytes, time.Since(start))
	}
	return errSend
}

func writeCodexWebsocketMessage(sess *codexWebsocketSession, conn *websocket.Conn, payload []byte) error {
	return writeWebsocketPayloadMessage("codex", sess, conn, payload)
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

// Preserve the transport cause while exposing an actionable, anonymous SSE error.
type codexWebsocketReadTimeoutError struct{ cause error }

func (e codexWebsocketReadTimeoutError) Error() string {
	return `{"error":{"message":"upstream websocket read timed out before response.completed","type":"server_error","code":"upstream_read_timeout"}}`
}
func (e codexWebsocketReadTimeoutError) Unwrap() error { return e.cause }

func mapCodexWebsocketReadError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return codexWebsocketReadTimeoutError{cause: err}
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

// codexWebsocketTurnTransportError turns a transport loss (EOF, reset, close 1006) into
// the turn error that keeps the downstream websocket open. Status, policy and other
// close codes are returned unchanged and keep ending the downstream socket.
func codexWebsocketTurnTransportError(err error) error {
	if err == nil || cliproxyexecutor.IsUpstreamWebsocketTurnError(err) {
		return err
	}
	var status interface{ StatusCode() int }
	if errors.As(err, &status) && status.StatusCode() != 0 {
		return err
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		if closeErr.Code != websocket.CloseAbnormalClosure {
			return err
		}
	} else if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) &&
		!errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) &&
		!errors.Is(err, syscall.ECONNRESET) && !errors.Is(err, syscall.ECONNABORTED) &&
		!errors.Is(err, syscall.EPIPE) {
		return err
	}
	return &cliproxyexecutor.UpstreamWebsocketTurnError{Cause: err}
}

// codexWebsocketTurnEventID lets a lost turn's error carry the create's event_id.
func codexWebsocketTurnEventID(err error, payload []byte) error {
	var turnErr *cliproxyexecutor.UpstreamWebsocketTurnError
	if !errors.As(err, &turnErr) || turnErr.EventID != "" {
		return err
	}
	withID := *turnErr
	withID.EventID = strings.Clone(ojson.GetBytesNoCopy(payload, "event_id").String())
	return &withID
}

func normalizeCodexWebsocketParallelToolCalls(body []byte, headers http.Header) []byte {
	if !util.IsCodexResponsesLiteRequest(body, headers) {
		return body
	}
	body = helps.SetBoolIfDifferent(body, "parallel_tool_calls", false)
	return body
}

func normalizeCodexWebsocketParallelToolCallsDoc(doc *ojson.Doc, headers http.Header) bool {
	if !isCodexResponsesLiteRequestDoc(doc, headers) {
		return false
	}
	return helps.SetBoolIfDifferentDoc(doc, "parallel_tool_calls", false)
}

func buildCodexWebsocketRequestBody(body []byte) []byte {
	return frameCodexWebsocketRequestBody(helps.SanitizeCodexInputItemIDs(body))
}

// Framing must not normalize the configured business payload.
func frameCodexWebsocketRequestBody(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	wsReqBody, errSet := ojson.SetBytes(body, "type", "response.create")
	if errSet == nil && len(wsReqBody) > 0 {
		return wsReqBody
	}
	return body
}

func readCodexWebsocketMessage(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn, readCh chan codexWebsocketRead) (int, []byte, error) {
	event := readCodexWebsocketEvent(ctx, sess, conn, readCh)
	return event.msgType, event.payload, event.err
}

func readCodexWebsocketEvent(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn, readCh chan codexWebsocketRead) codexWebsocketRead {
	if ctx == nil {
		ctx = context.Background()
	}
	if sess == nil {
		if conn == nil {
			return codexWebsocketRead{err: fmt.Errorf("codex websockets executor: websocket conn is nil")}
		}
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		// This connection belongs to one request. Interrupt ReadMessage when
		// its downstream cancels instead of waiting for the liveness deadline.
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

func executionProxyURL(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) string {
	proxyURL := cliproxyexecutor.RequestProxyURL(ctx)
	if proxyURL == "" && auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	return proxyURL
}

func newProxyAwareWebsocketDialer(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) *websocket.Dialer {
	dialer := &websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  codexResponsesWebsocketHandshakeTO,
		EnableCompression: true,
		NetDialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	proxyURL := executionProxyURL(ctx, cfg, auth)
	if proxyURL == "" {
		return dialer
	}

	proxyDialer, mode, err := proxyutil.BuildDialer(proxyURL)
	if err != nil || (mode == proxyutil.ModeProxy && !proxyutil.ValidRequestProxy(proxyURL)) {
		dialer.Proxy = nil
		dialer.NetDialContext = func(context.Context, string, string) (net.Conn, error) {
			return nil, errInvalidWebsocketProxy
		}
		return dialer
	}
	if mode == proxyutil.ModeDirect {
		dialer.Proxy = nil
	} else if mode == proxyutil.ModeProxy {
		dialer.Proxy = nil
		if contextual, ok := proxyDialer.(proxy.ContextDialer); ok {
			dialer.NetDialContext = contextual.DialContext
		} else {
			dialer.NetDialContext = func(context.Context, string, string) (net.Conn, error) {
				return nil, errInvalidWebsocketProxy
			}
		}
	}
	return dialer
}

var errInvalidWebsocketProxy = errors.New("invalid websocket proxy configuration")

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
