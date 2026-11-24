package executor

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
)

type ordinaryCodexWebsocketTurn struct {
	session    *codexWebsocketSession
	connection *websocket.Conn
	read       chan codexWebsocketRead
	release    func()
	body       []byte
	prepared   *codexWebsocketPrepared
	headers    http.Header
	ephemeral  bool
	reporter   *helps.UsageReporter
}

func ordinaryWebsocketScope(tenant, authID, token, grant, base, route string) string {
	mac := hmac.New(sha256.New, []byte(tenant))
	for _, field := range []string{authID, token, grant, base, route} {
		_, _ = io.WriteString(mac, field)
		_, _ = mac.Write([]byte{0})
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func acquireOrdinaryCodexTurn(ctx context.Context, sess *codexWebsocketSession, scope, authID string) (func(), error) {
	sess.connMu.Lock()
	if sess.closed {
		sess.connMu.Unlock()
		return nil, errCodexWebsocketSessionClosed
	}
	if sess.ordinaryOwner == "" {
		sess.ordinaryOwner = scope
		sess.ordinaryAuthID = strings.TrimSpace(authID)
	}
	if sess.ordinaryOwner != scope {
		sess.connMu.Unlock()
		return nil, statusErr{code: http.StatusConflict, msg: "Codex WebSocket session belongs to a different credential scope", requestScoped: true}
	}
	if sess.ordinaryTurn == nil {
		sess.ordinaryTurn = make(chan struct{}, 1)
	}
	gate := sess.ordinaryTurn
	closed := sess.closedContext
	sess.connMu.Unlock()
	var done <-chan struct{}
	if closed != nil {
		done = closed.Done()
	}
	select {
	case <-done:
		return nil, errCodexWebsocketSessionClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	case gate <- struct{}{}:
		release := func() { <-gate }
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		if closed != nil && closed.Err() != nil {
			release()
			return nil, errCodexWebsocketSessionClosed
		}
		return release, nil
	}
}

func (e *CodexWebsocketsExecutor) startOrdinaryCodexWebsocket(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*ordinaryCodexWebsocketTurn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if auth == nil || auth.Disabled {
		return nil, statusErr{code: http.StatusForbidden, msg: "Codex credential is missing or disabled", requestScoped: true}
	}
	route := executionProxyURL(ctx, e.cfg, auth)
	routeSetting, err := proxyutil.Parse(route)
	if route == "" || err != nil || (routeSetting.Mode == proxyutil.ModeProxy && !proxyutil.ValidRequestProxy(route)) {
		return nil, errInvalidWebsocketProxy
	}
	// Reuse and dialing must use the same authorized route for this turn.
	ctx = cliproxyexecutor.WithRequestProxyURL(ctx, route)
	model := thinking.ParseSuffix(req.Model).ModelName
	token, base := ordinaryCodexImageCredential(auth)
	sessionID := executionSessionIDFromOptions(opts)
	tenant := strings.TrimSpace(helps.APIKeyFromContext(ctx))
	ephemeral := sessionID == "" || tenant == ""
	input := ojson.NewDoc(req.Payload)
	if input.Get("type").String() == "response.append" || input.Get("previous_response_id").String() != "" {
		ctx = cliproxyexecutor.WithRequiredUpstreamWebsocket(ctx)
	}
	if ephemeral && cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return nil, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
	}
	var sess *codexWebsocketSession
	if ephemeral {
		sess = newEphemeralCodexWebsocketSession()
	} else {
		sess = e.getOrCreateSession(sessionID)
	}
	release, err := acquireOrdinaryCodexTurn(ctx, sess, ordinaryWebsocketScope(tenant, auth.ID, token, ordinaryCodexMetadataString(auth.Metadata, "refresh_token"), base, route), auth.ID)
	if err != nil {
		return nil, err
	}

	prepared, err := e.prepareCodexWebsocketStream(ctx, auth, req, opts, sess)
	if err != nil {
		release()
		return nil, err
	}
	wsURL := prepared.wsURL

	// The selected credential and standard headers are fixed throughout this turn.
	conn, closer, resp, err := e.ensureUpstreamConn(ctx, auth, sess, auth.ID, wsURL, prepared.wsHeaders)
	if err != nil {
		release()
		if resp != nil {
			defer resp.Body.Close()
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			logHeaders := make(http.Header)
			for _, name := range []string{"Content-Type", "Retry-After", "X-Request-Id"} {
				for _, value := range resp.Header.Values(name) {
					logHeaders.Add(name, value)
				}
			}
			helps.RecordAPIWebsocketUpgradeRejection(ctx, e.cfg, helps.UpstreamRequestLog{
				URL: wsURL, Method: http.MethodGet, Provider: e.Identifier(),
			}, resp.StatusCode, logHeaders, raw)
			return nil, newCodexStatusErrWithHeadersAndCooling(resp.StatusCode, resp.Header, raw, e.modelLevelCooling())
		}
		return nil, err
	}
	recordAPIWebsocketHandshake(ctx, e.cfg, resp)
	if err = sess.bindExecutionLifecycle(opts, conn, closer, model); err != nil {
		closeWebsocketAfterBindFailure(sess, conn, closer)
		release()
		return nil, err
	}
	if err := prepared.sessionInstructions.OnConnection(conn); err != nil {
		e.invalidateUpstreamConn(sess, conn, "instruction_connection_changed", err)
		release()
		return nil, err
	}
	ch := sess.activate(conn)
	var headers http.Header
	if resp != nil {
		headers = resp.Header.Clone()
	}
	turn := &ordinaryCodexWebsocketTurn{session: sess, connection: conn, read: ch, release: release, body: prepared.clientBody, prepared: prepared, headers: headers, ephemeral: ephemeral,
		reporter: helps.NewExecutorUsageReporter(ctx, e.CodexExecutor, model, auth)}
	cliproxyexecutor.MarkUpstreamAttempt(ctx)
	// A structured request is sent at most once, even when the peer rejects it.
	wire := prepared.clientBody
	if ojson.GetBytesNoCopy(wire, "type").String() != "response.append" {
		wire = frameCodexWebsocketRequestBody(wire)
	}
	if err = writeCodexWebsocketMessage(sess, conn, wire); err != nil {
		err = mapCodexWebsocketWriteError(sess, conn, err)
		e.invalidateUpstreamConn(sess, conn, "send_error", err)
		e.finishOrdinaryCodexWebsocket(turn, err)
		return nil, err
	}
	prepared.sessionReceipt = sess.sessionInstructions.Sent(conn, prepared.sessionInstructions)
	return turn, nil
}

func (e *CodexWebsocketsExecutor) finishOrdinaryCodexWebsocket(turn *ordinaryCodexWebsocketTurn, err error) {
	turn.session.clearActive(turn.connection, turn.read)
	if err != nil {
		e.invalidateUpstreamConn(turn.session, turn.connection, "ordinary_turn_ended", err)
	}
	if turn.ephemeral {
		closeCodexWebsocketSession(turn.session, "ordinary_turn_ended")
	}
	turn.release()
}

func nextOrdinaryCodexWebsocketPayload(ctx context.Context, turn *ordinaryCodexWebsocketTurn) ([]byte, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case read, ok := <-turn.read:
			if !ok {
				return nil, newCodexIncompleteStreamError()
			}
			if read.conn != turn.connection {
				continue
			}
			if read.err != nil {
				return nil, read.err
			}
			if read.msgType != websocket.TextMessage {
				continue
			}
			if !ojson.ValidBytes(read.payload) {
				return nil, statusErr{code: http.StatusBadGateway, msg: "malformed Codex WebSocket JSON"}
			}
			return read.payload, nil
		}
	}
}
