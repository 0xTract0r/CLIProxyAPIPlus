package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type codexWebsocketSessionStore struct {
	mu            sync.Mutex
	sessions      map[string]*codexWebsocketSession
	disconnects   map[string]*codexWebsocketSession
	leaseChanged  chan struct{}
	extraByTarget map[string]int
	extraCount    int
	leaseSequence uint64
}

const codexWebsocketExtraPerTarget = 1
const codexWebsocketExtraGlobal = 8

var errCodexWebsocketSessionClosed = fmt.Errorf("codex websocket session closed")

type codexWebsocketSessionLease struct {
	once                sync.Once
	store               *codexWebsocketSessionStore
	session             *codexWebsocketSession
	storeKey, targetKey string
	extra               bool
}

func (s *codexWebsocketSessionStore) notifyLeaseChangedLocked() {
	if s.leaseChanged != nil {
		close(s.leaseChanged)
	}
	s.leaseChanged = make(chan struct{})
}

func (l *codexWebsocketSessionLease) release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		// Return a permit only after its connection has been closed.
		if l.extra {
			closeCodexWebsocketSession(l.session, "concurrent_lease_released")
		}
		l.session.reqMu.Unlock()
		l.store.mu.Lock()
		if l.extra {
			if l.store.sessions[l.storeKey] == l.session {
				delete(l.store.sessions, l.storeKey)
			}
			l.store.extraCount--
			l.store.extraByTarget[l.targetKey]--
			if l.store.extraByTarget[l.targetKey] == 0 {
				delete(l.store.extraByTarget, l.targetKey)
			}
		}
		l.store.notifyLeaseChangedLocked()
		l.store.mu.Unlock()
	})
}

func codexFastIndependentRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, body []byte, fast bool) bool {
	independent, reason := codexFastIndependentRequestEligibility(ctx, req, opts, body, fast)
	helps.LogWithRequestID(ctx).Infof("codex websocket lease eligibility independent=%t reason=%s", independent, reason)
	return independent
}

func codexFastIndependentRequestEligibility(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, body []byte, fast bool) (bool, string) {
	if !fast {
		return false, "fast_disabled"
	}
	if executionSessionIDFromOptions(opts) != "" {
		return false, "execution_session"
	}
	if !codexHTTPFallbackAllowed(ctx, req, opts, body) {
		return false, "bound_or_incremental_request"
	}
	for _, payload := range [][]byte{req.Payload, opts.OriginalRequest, body} {
		if gjson.GetBytes(payload, "multi_agent.enabled").Bool() {
			return false, "active_multi_agent"
		}
		for _, key := range []string{"conversation", "conversation_id", "response_id", "stream_id"} {
			if gjson.GetBytes(payload, key).Exists() {
				return false, "connection_anchor"
			}
		}
	}
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		if strings.TrimSpace(input.String()) != "" {
			return true, "complete_input"
		}
		return false, "input_shape"
	}
	if !input.IsArray() || len(input.Array()) == 0 {
		return false, "input_shape"
	}
	pending := make(map[string]bool)
	for _, item := range input.Array() {
		switch item.Get("type").String() {
		case "item_reference":
			return false, "input_reference"
		case "function_call", "custom_tool_call":
			id := item.Get("call_id").String()
			if id == "" || pending[id] {
				return false, "tool_dependency"
			}
			pending[id] = true
		case "function_call_output", "custom_tool_call_output":
			id := item.Get("call_id").String()
			if !pending[id] {
				return false, "tool_dependency"
			}
			delete(pending, id)
		case "message", "reasoning":
		case "compaction":
			if !codexHistoryNonemptyString(item.Get("encrypted_content")) {
				return false, "compaction_shape"
			}
		case "agent_message":
			if !codexIndependentAgentMessage(item) {
				return false, "agent_message_shape"
			}
		case "additional_tools":
			if item.Get("role").String() != "developer" || item.Get("role").Type != gjson.String || !codexIndependentToolDeclarations(item.Get("tools")) {
				return false, "additional_tools_shape"
			}
		case "":
			if !item.IsObject() || !item.Get("role").Exists() || !item.Get("content").Exists() {
				return false, "input_shape"
			}
		default:
			// Unverified tool/approval items may depend on state held by the base socket.
			return false, "unknown_input_item"
		}
	}
	if len(pending) != 0 {
		return false, "tool_dependency"
	}
	return true, "complete_input"
}

func codexHistoryNonemptyString(value gjson.Result) bool {
	return value.Type == gjson.String && strings.TrimSpace(value.String()) != ""
}

func codexIndependentAgentMessage(item gjson.Result) bool {
	if !codexHistoryNonemptyString(item.Get("author")) || !codexHistoryNonemptyString(item.Get("recipient")) {
		return false
	}
	content := item.Get("content")
	if !content.IsArray() || len(content.Array()) == 0 {
		return false
	}
	for _, part := range content.Array() {
		switch part.Get("type").String() {
		case "input_text":
			if part.Get("text").Type != gjson.String {
				return false
			}
		case "encrypted_content":
			if !codexHistoryNonemptyString(part.Get("encrypted_content")) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func codexIndependentToolDeclarations(tools gjson.Result) bool {
	if !tools.IsArray() || len(tools.Array()) == 0 {
		return false
	}
	for _, tool := range tools.Array() {
		if !codexHistoryNonemptyString(tool.Get("name")) {
			return false
		}
		if description := tool.Get("description"); description.Exists() && description.Type != gjson.String {
			return false
		}
		if tool.Get("type").String() == "namespace" {
			children := tool.Get("tools")
			if !children.IsArray() || len(children.Array()) == 0 {
				return false
			}
			for _, child := range children.Array() {
				if !codexIndependentLeafToolDeclaration(child) {
					return false
				}
			}
		} else if !codexIndependentLeafToolDeclaration(tool) {
			return false
		}
	}
	return true
}

func codexIndependentLeafToolDeclaration(tool gjson.Result) bool {
	if !codexHistoryNonemptyString(tool.Get("name")) {
		return false
	}
	if description := tool.Get("description"); description.Exists() && description.Type != gjson.String {
		return false
	}
	switch tool.Get("type").String() {
	case "function":
		return tool.Get("parameters").IsObject()
	case "custom":
		format := tool.Get("format")
		switch format.Get("type").String() {
		case "text":
			return true
		case "grammar":
			return codexHistoryNonemptyString(format.Get("definition")) && (format.Get("syntax").String() == "lark" || format.Get("syntax").String() == "regex")
		}
	}
	return false
}

func (e *CodexWebsocketsExecutor) acquireSessionLease(ctx context.Context, sessionID string, auth *cliproxyauth.Auth, wsURL string, independent bool) (*codexWebsocketSessionLease, error) {
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return e.acquireBoundContinuationLease(ctx, sessionID, auth, wsURL)
	}
	proxy := executionProxyURL(ctx, e.cfg, auth)
	base := e.getOrCreateSession(sessionID, auth, wsURL, proxy)
	if base == nil {
		return nil, nil
	}
	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}
	baseKey := codexWebsocketSessionStoreKey(sessionID, auth, wsURL, proxy)
	targetKey := codexWebsocketSessionStoreKey("fast-concurrent-target", auth, wsURL, proxy)
	started := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		store.mu.Lock()
		if store.leaseChanged == nil {
			store.leaseChanged = make(chan struct{})
		}
		changed := store.leaseChanged // Capture before checking availability: no lost wakeups.
		if store.sessions[baseKey] != base {
			store.mu.Unlock()
			return nil, errCodexWebsocketSessionClosed
		}
		if base.reqMu.TryLock() {
			store.mu.Unlock()
			lease := &codexWebsocketSessionLease{store: store, session: base}
			if err := ctx.Err(); err != nil {
				lease.release()
				return nil, err
			}
			helps.LogWithRequestID(ctx).Infof("codex fast websocket lease mode=base wait_ms=%d", time.Since(started).Milliseconds())
			return lease, nil
		}
		if independent && store.extraCount < codexWebsocketExtraGlobal && store.extraByTarget[targetKey] < codexWebsocketExtraPerTarget {
			if store.extraByTarget == nil {
				store.extraByTarget = make(map[string]int)
			}
			store.leaseSequence++
			key := fmt.Sprintf("%s|extra:%d", baseKey, store.leaseSequence)
			authID := ""
			if auth != nil {
				authID = auth.ID
			}
			sess := newCodexWebsocketSession(sessionID, authID, wsURL)
			sess.disconnectOwner = base.disconnectOwner
			sess.reqMu.Lock()
			store.sessions[key] = sess
			store.extraCount++
			store.extraByTarget[targetKey]++
			store.mu.Unlock()
			lease := &codexWebsocketSessionLease{store: store, session: sess, storeKey: key, targetKey: targetKey, extra: true}
			if err := ctx.Err(); err != nil {
				lease.release()
				return nil, err
			}
			helps.LogWithRequestID(ctx).Infof("codex fast websocket lease mode=extra wait_ms=%d", time.Since(started).Milliseconds())
			return lease, nil
		}
		store.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

// A required continuation may borrow only its live owner binding. A global
// config reload changes future dials, never the route of this existing socket.
// No fallback dial is authorized here. Keep the close order: owner fence -> store.
func (e *CodexWebsocketsExecutor) acquireBoundContinuationLease(ctx context.Context, sessionID string, auth *cliproxyauth.Auth, wsURL string) (*codexWebsocketSessionLease, error) {
	replay := func() (*codexWebsocketSessionLease, error) {
		return nil, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
	}
	if auth == nil || strings.TrimSpace(sessionID) == "" {
		return replay()
	}
	store := e.sessionStore()
	store.mu.Lock()
	owner := store.disconnects[strings.TrimSpace(sessionID)]
	store.mu.Unlock()
	if owner == nil {
		return replay()
	}
	// Pin the observed response-state generation before any wait. A wakeup may
	// only retry this binding; a reopened logical ID owns different history.
	owner.connMu.Lock()
	sess, generation := owner.activeDisconnectSession, owner.activeDisconnectConn
	owner.connMu.Unlock()
	if sess == nil || generation == nil {
		return replay()
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := lockCodexWebsocketContext(ctx, &owner.interruptFence); err != nil {
			return nil, err
		}
		store.mu.Lock()
		if store.leaseChanged == nil {
			store.leaseChanged = make(chan struct{})
		}
		changed := store.leaseChanged
		owner.connMu.Lock()
		sameGeneration := !owner.closed && owner.activeDisconnectSession == sess && owner.activeDisconnectConn == generation
		owner.connMu.Unlock()
		valid := store.disconnects[strings.TrimSpace(sessionID)] == owner && sameGeneration
		acquired := false
		if valid {
			sess.connMu.Lock()
			key := codexWebsocketSessionStoreKey(sessionID, auth, wsURL, sess.proxyURL)
			valid = !sess.closed && sess.conn == generation && sess.connCloser != nil && sess.hasConnCfg &&
				sess.connEpoch == auth.RegistrationEpoch && sess.connProvider == auth.Provider && sess.authID == auth.ID && sess.wsURL == wsURL &&
				sess.connAuthProxy == strings.TrimSpace(auth.ProxyURL) && sess.connRequestProxy == cliproxyexecutor.RequestProxyURL(ctx) &&
				store.sessions[key] == sess
			if valid {
				acquired = sess.reqMu.TryLock()
			}
			sess.connMu.Unlock()
		}
		store.mu.Unlock()
		owner.interruptFence.Unlock()
		if !valid {
			return replay()
		}
		if acquired {
			lease := &codexWebsocketSessionLease{store: store, session: sess}
			if err := ctx.Err(); err != nil {
				lease.release()
				return nil, err
			}
			return lease, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

var globalCodexWebsocketSessionStore = &codexWebsocketSessionStore{
	sessions: make(map[string]*codexWebsocketSession),
}

type websocketConnectionCloser struct {
	conn *websocket.Conn
	once sync.Once
	err  error
}

func newWebsocketConnectionCloser(conn *websocket.Conn) *websocketConnectionCloser {
	if conn == nil {
		return nil
	}
	return &websocketConnectionCloser{conn: conn}
}

func (c *websocketConnectionCloser) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	c.once.Do(func() {
		c.err = c.conn.Close()
	})
	return c.err
}

type codexWebsocketSession struct {
	sessionID string

	reqMu sync.Mutex

	connMu                    sync.Mutex
	conn                      *websocket.Conn
	connCloser                *websocketConnectionCloser
	wsURL                     string
	authID                    string
	proxyURL                  string
	useSIWC                   bool
	connProvider              string
	connEpoch                 uint64
	connAuthProxy             string
	connRequestProxy          string
	connCfg                   *config.Config
	hasConnCfg                bool
	multiAgentV2OptimizedConn *websocket.Conn
	lifecycleBindMu           sync.Mutex
	lifecycle                 cliproxyexecutor.ExecutionLifecycle
	lifecycleModel            string
	closed                    bool
	closedContext             context.Context
	closeCancel               context.CancelFunc
	disconnectOwner           *codexWebsocketSession
	activeDisconnectSession   *codexWebsocketSession
	activeDisconnectConn      *websocket.Conn

	writeMu sync.Mutex
	// 逻辑 owner 的 fence：串行化 generation 变更与 interrupt 最终写出。
	interruptFence sync.Mutex

	activeMu     sync.Mutex
	activeConn   *websocket.Conn
	activeCh     chan codexWebsocketRead
	activeDone   <-chan struct{}
	activeCancel context.CancelFunc
	terminalConn *websocket.Conn
	terminalErr  error

	responseStateConn   *websocket.Conn
	terminalResponseIDs map[string]struct{}

	readerConn *websocket.Conn

	upstreamDisconnectOnce    sync.Once
	upstreamDisconnectCh      chan error
	upstreamDisconnectErrMu   sync.RWMutex
	upstreamDisconnectErrConn *websocket.Conn
	upstreamDisconnectErr     error

	lastEventMu   sync.Mutex
	lastEventConn *websocket.Conn
	lastEventType string
}

func newCodexWebsocketSession(sessionID, authID, wsURL string) *codexWebsocketSession {
	closedContext, closeCancel := context.WithCancel(context.Background())
	return &codexWebsocketSession{sessionID: sessionID, authID: authID, wsURL: wsURL, closedContext: closedContext, closeCancel: closeCancel, upstreamDisconnectCh: make(chan error, 1)}
}

type codexWebsocketRead struct {
	conn         *websocket.Conn
	msgType      int
	payload      []byte
	err          error
	arrival      time.Time
	readDuration time.Duration
}

func (s *codexWebsocketSession) setActive(conn *websocket.Conn, ch chan codexWebsocketRead) {
	if s == nil {
		return
	}
	s.activeMu.Lock()
	s.setActiveLocked(conn, ch)
	s.activeMu.Unlock()
}

func (s *codexWebsocketSession) setActiveLocked(conn *websocket.Conn, ch chan codexWebsocketRead) {
	if s.activeCancel != nil {
		s.activeCancel()
		s.activeCancel = nil
		s.activeDone = nil
	}
	s.activeConn = conn
	s.activeCh = ch
	if conn != nil && ch != nil {
		activeCtx, activeCancel := context.WithCancel(context.Background())
		s.activeDone = activeCtx.Done()
		s.activeCancel = activeCancel
	}
}

func (s *codexWebsocketSession) activate(conn *websocket.Conn) chan codexWebsocketRead {
	if s == nil || conn == nil {
		return nil
	}
	ch := make(chan codexWebsocketRead, 4096)
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if s.terminalConn == conn && s.terminalErr != nil {
		ch <- codexWebsocketRead{conn: conn, err: s.terminalErr}
		close(ch)
		return ch
	}
	s.setActiveLocked(conn, ch)
	return ch
}

func (s *codexWebsocketSession) resetTerminalError(conn *websocket.Conn) {
	if s == nil || conn == nil {
		return
	}
	s.activeMu.Lock()
	s.terminalConn = conn
	s.terminalErr = nil
	if s.responseStateConn != conn {
		s.responseStateConn = conn
		s.terminalResponseIDs = make(map[string]struct{})
	}
	s.activeMu.Unlock()
}

func (s *codexWebsocketSession) markTerminalError(conn *websocket.Conn, err error) bool {
	if s == nil || conn == nil || err == nil {
		return false
	}
	s.connMu.Lock()
	if s.conn != conn {
		s.connMu.Unlock()
		return false
	}
	s.activeMu.Lock()
	s.terminalConn = conn
	s.terminalErr = err
	s.activeMu.Unlock()
	s.connMu.Unlock()
	return true
}

func (s *codexWebsocketSession) activeForConn(conn *websocket.Conn) (chan codexWebsocketRead, <-chan struct{}) {
	if s == nil || conn == nil {
		return nil, nil
	}
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if s.activeConn != conn {
		return nil, nil
	}
	return s.activeCh, s.activeDone
}

func clearRetryActiveState(sess *codexWebsocketSession, conn *websocket.Conn, ch chan codexWebsocketRead) bool {
	if sess == nil {
		return false
	}
	return sess.clearActive(conn, ch)
}

func (s *codexWebsocketSession) clearActive(conn *websocket.Conn, ch chan codexWebsocketRead) bool {
	if s == nil {
		return false
	}
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if s.activeConn != conn || s.activeCh != ch {
		return false
	}
	s.activeConn = nil
	s.activeCh = nil
	if s.activeCancel != nil {
		s.activeCancel()
	}
	s.activeCancel = nil
	s.activeDone = nil
	return true
}

const codexWebsocketWriteChunkSize = 32 * 1024

var (
	testWebsocketWritePayloadHook func(conn *websocket.Conn)
	testWebsocketWriteChunkHook   func(chunkIndex int, totalChunks int)
)

func (s *codexWebsocketSession) writeMessage(conn *websocket.Conn, msgType int, payload []byte) error {
	if s == nil {
		return fmt.Errorf("codex websockets executor: session is nil")
	}
	if conn == nil {
		return fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if testWebsocketWritePayloadHook != nil {
		testWebsocketWritePayloadHook(conn)
	}
	return s.writePayload(conn, msgType, payload)
}

// 调用者持 writeMu；interrupt 还持 owner fence，但不持 connMu/activeMu。
func (s *codexWebsocketSession) writePayload(conn *websocket.Conn, msgType int, payload []byte) error {
	if len(payload) <= codexWebsocketWriteChunkSize {
		return conn.WriteMessage(msgType, payload)
	}
	w, errNext := conn.NextWriter(msgType)
	if errNext != nil {
		return errNext
	}
	totalChunks := (len(payload) + codexWebsocketWriteChunkSize - 1) / codexWebsocketWriteChunkSize
	for i := 0; i < len(payload); i += codexWebsocketWriteChunkSize {
		end := i + codexWebsocketWriteChunkSize
		if end > len(payload) {
			end = len(payload)
		}
		chunkIdx := i / codexWebsocketWriteChunkSize
		if testWebsocketWriteChunkHook != nil {
			testWebsocketWriteChunkHook(chunkIdx, totalChunks)
		}
		if _, errWrite := w.Write(payload[i:end]); errWrite != nil {
			_ = w.Close()
			return errWrite
		}
	}
	return w.Close()
}

// 固定控制帧预算包括等 writer、等 fence 和实际 socket 写，不新增用户开关。
const codexWebsocketInterruptWriteTimeout = 5 * time.Second

func lockCodexWebsocketContext(ctx context.Context, mu *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if mu.TryLock() {
		return nil
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if err := ctx.Err(); err != nil {
				return err
			}
			if mu.TryLock() {
				return nil
			}
		}
	}
}

func (s *codexWebsocketSession) lifecycleFence() *sync.Mutex {
	if s.disconnectOwner != nil {
		return &s.disconnectOwner.interruptFence
	}
	return &s.interruptFence
}

func (s *codexWebsocketSession) writeInterrupt(ctx context.Context, owner *codexWebsocketSession, conn *websocket.Conn, payload []byte, authEnabled bool) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, codexWebsocketInterruptWriteTimeout)
	defer cancel()
	if err := lockCodexWebsocketContext(ctx, &s.writeMu); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	if testWebsocketWritePayloadHook != nil {
		testWebsocketWritePayloadHook(conn)
	}
	if err := lockCodexWebsocketContext(ctx, &owner.interruptFence); err != nil {
		return false, err
	}
	defer owner.interruptFence.Unlock()
	// 锁序：writeMu -> lifecycle fence -> physical.connMu -> owner.connMu -> activeMu。
	// bind/close/detach 先取同一 fence，绝不持状态锁等待 fence 或网络 I/O。
	s.connMu.Lock()
	owner.connMu.Lock()
	if s.closed || s.conn != conn || owner.closed || owner.activeDisconnectSession != s || owner.activeDisconnectConn != conn {
		owner.connMu.Unlock()
		s.connMu.Unlock()
		return false, cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	closer := s.connCloser
	s.activeMu.Lock()
	_, terminal := s.terminalResponseIDs[gjson.GetBytes(payload, "response_id").String()]
	terminal = terminal && s.responseStateConn == conn
	active := s.activeConn == conn && s.activeCh != nil
	var disconnectErr error
	if s.terminalConn == conn {
		disconnectErr = s.terminalErr
	}
	s.activeMu.Unlock()
	owner.connMu.Unlock()
	s.connMu.Unlock()
	if disconnectErr != nil {
		return false, disconnectErr
	}
	if terminal {
		return false, nil
	}
	if !active {
		return false, cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	if !authEnabled {
		return false, fmt.Errorf("websocket credential is no longer enabled")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	deadline, _ := ctx.Deadline()
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return false, err
	}
	defer conn.SetWriteDeadline(time.Time{})
	// Close 可与 Gorilla writer 并发，SetWriteDeadline 不可；取消关闭当前物理连接，
	// 等回调结束后再放 fence，避免迟到取消影响后继 generation。
	cancelDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		if closer != nil {
			_ = closer.Close()
		} else {
			_ = conn.Close()
		}
		close(cancelDone)
	})
	defer func() {
		if !stop() {
			<-cancelDone
		}
	}()
	err := s.writePayload(conn, websocket.TextMessage, payload)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return err == nil, err
}

func (s *codexWebsocketSession) setMultiAgentV2Optimized(conn *websocket.Conn, optimized bool) {
	if s == nil || conn == nil {
		return
	}
	s.connMu.Lock()
	if s.conn == conn {
		if optimized {
			s.multiAgentV2OptimizedConn = conn
		} else {
			s.multiAgentV2OptimizedConn = nil
		}
	}
	s.connMu.Unlock()
}

func (s *codexWebsocketSession) isMultiAgentV2Optimized(conn *websocket.Conn) bool {
	if s == nil || conn == nil {
		return false
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return s.conn == conn && s.multiAgentV2OptimizedConn == conn
}

// sendTerminalWebsocketRead reports whether it invalidated a full channel's connection before waiting.
func sendTerminalWebsocketRead(ch chan<- codexWebsocketRead, done <-chan struct{}, event codexWebsocketRead, invalidate func()) bool {
	select {
	case ch <- event:
		return false
	case <-done:
		return false
	default:
	}

	invalidated := invalidate != nil
	if invalidated {
		invalidate()
	}
	select {
	case ch <- event:
	case <-done:
	}
	return invalidated
}

func (s *codexWebsocketSession) configureConn(conn *websocket.Conn) {
	if s == nil || conn == nil {
		return
	}
	s.resetTerminalError(conn)
	s.resetUpstreamDisconnectError(conn)
	conn.SetPingHandler(func(appData string) error {
		sessionID := ""
		if s != nil {
			sessionID = s.sessionID
		}
		sessionKind := sessionObjectKind(s)
		log.Debugf("codex websockets: upstream ping received session=%s session_object=%s ping_bytes=%d", sessionID, sessionKind, len(appData))
		log.Debugf("codex websockets: upstream pong write started session=%s session_object=%s", sessionID, sessionKind)
		start := time.Now()
		// Gorilla websocket allows concurrent WriteControl with WriteMessage.
		// Avoid writeMu here so keepalive pongs are not starved by long payload writes.
		errPong := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
		if errPong != nil {
			log.Warnf("codex websockets: upstream pong write failed session=%s session_object=%s duration=%v err=%v", sessionID, sessionKind, time.Since(start), errPong)
		} else {
			log.Debugf("codex websockets: upstream pong replied session=%s session_object=%s duration=%v", sessionID, sessionKind, time.Since(start))
		}
		return errPong
	})
	defaultCloseHandler := conn.CloseHandler()
	conn.SetCloseHandler(func(code int, text string) error {
		s.setUpstreamDisconnectError(conn, &websocket.CloseError{Code: code, Text: text})
		return defaultCloseHandler(code, text)
	})
}

func (s *codexWebsocketSession) bindExecutionLifecycle(opts cliproxyexecutor.Options, conn *websocket.Conn, closer *websocketConnectionCloser, model string) error {
	if closer == nil {
		return fmt.Errorf("codex websockets executor: websocket connection closer is nil")
	}
	if s == nil {
		return cliproxyexecutor.BindExecutionResource(opts, closer)
	}
	lifecycle := opts.ExecutionLifecycle
	if lifecycle == nil || conn == nil {
		return nil
	}

	s.lifecycleBindMu.Lock()
	defer s.lifecycleBindMu.Unlock()

	s.connMu.Lock()
	if s.conn == conn && s.connCloser == nil {
		s.connCloser = closer
	}
	alreadyBound := s.conn == conn && s.connCloser == closer && s.lifecycle == lifecycle
	s.connMu.Unlock()
	if alreadyBound {
		return nil
	}

	if errBind := lifecycle.Bind(func() error {
		return s.closeBoundConnection(conn, closer, lifecycle)
	}); errBind != nil {
		return errBind
	}
	if retained, ok := lifecycle.(interface{ Retain() }); ok {
		retained.Retain()
	}

	s.connMu.Lock()
	if s.conn != conn || s.connCloser != closer {
		s.connMu.Unlock()
		return fmt.Errorf("codex websockets executor: websocket connection closed during lifecycle bind")
	}
	previous := s.lifecycle
	s.lifecycle = lifecycle
	s.lifecycleModel = strings.TrimSpace(model)
	s.connMu.Unlock()
	if previous != nil && previous != lifecycle {
		previous.End("target_replaced")
	}
	return nil
}

func (s *codexWebsocketSession) closeBoundConnection(conn *websocket.Conn, closer *websocketConnectionCloser, lifecycle cliproxyexecutor.ExecutionLifecycle) error {
	if s == nil || conn == nil {
		return nil
	}
	s.detachConnection(conn, lifecycle)
	errClose := closer.Close()
	go lifecycle.End("connection_closed")
	return errClose
}

func (s *codexWebsocketSession) detachConnection(conn *websocket.Conn, lifecycle cliproxyexecutor.ExecutionLifecycle) *websocketConnectionCloser {
	if s == nil || conn == nil {
		return nil
	}
	fence := s.lifecycleFence()
	fence.Lock()
	defer fence.Unlock()
	s.connMu.Lock()
	var closer *websocketConnectionCloser
	matched := s.conn == conn
	if matched {
		closer = s.connCloser
		s.conn = nil
		s.connCloser = nil
		s.connCfg = nil
		s.hasConnCfg = false
		s.multiAgentV2OptimizedConn = nil
		if s.readerConn == conn {
			s.readerConn = nil
		}
	}
	if (lifecycle == nil && matched) || (lifecycle != nil && s.lifecycle == lifecycle) {
		s.lifecycle = nil
		s.lifecycleModel = ""
	}
	s.connMu.Unlock()
	return closer
}

func closeWebsocketAfterBindFailure(sess *codexWebsocketSession, conn *websocket.Conn, closer *websocketConnectionCloser) {
	if conn == nil || closer == nil {
		return
	}
	if sess != nil {
		sess.detachConnection(conn, nil)
	}
	if errClose := closer.Close(); errClose != nil {
		log.Errorf("websockets executor: close lifecycle bind failure connection error: %v", errClose)
	}
}

func websocketSessionTargetChanged(sess *codexWebsocketSession, authID string, wsURL string, proxyURL string, useSIWC ...bool) bool {
	if sess == nil {
		return false
	}

	sess.connMu.Lock()
	defer sess.connMu.Unlock()
	if strings.TrimSpace(sess.authID) == "" && strings.TrimSpace(sess.wsURL) == "" {
		return false
	}
	return !websocketSessionTargetMatches(sess, authID, wsURL, proxyURL, useSIWC...)
}

func existingWebsocketSessionConn(sess *codexWebsocketSession, authID string, wsURL string, proxyURL string, useSIWC ...bool) (*websocket.Conn, *websocketConnectionCloser) {
	if sess == nil {
		return nil, nil
	}
	sess.connMu.Lock()
	conn := sess.conn
	closer := sess.connCloser
	matches := conn != nil && closer != nil && websocketSessionTargetMatches(sess, authID, wsURL, proxyURL, useSIWC...)
	sess.connMu.Unlock()
	if !matches || sess.upstreamDisconnectError(conn) != nil {
		return nil, nil
	}
	return conn, closer
}

func (s *codexWebsocketSession) effectiveConfig(authID string, fallback *config.Config) *config.Config {
	if s == nil {
		return fallback
	}
	s.connMu.Lock()
	sameConn := s.conn != nil && s.hasConnCfg && strings.TrimSpace(s.authID) == strings.TrimSpace(authID)
	cfg := s.connCfg
	s.connMu.Unlock()
	if sameConn {
		return cfg
	}
	return fallback
}

func websocketSessionTargetMatches(sess *codexWebsocketSession, authID string, wsURL string, proxyURL string, useSIWC ...bool) bool {
	if len(useSIWC) > 0 && sess.useSIWC != useSIWC[0] {
		return false
	}
	return strings.TrimSpace(sess.authID) == strings.TrimSpace(authID) &&
		strings.TrimSpace(sess.wsURL) == strings.TrimSpace(wsURL) &&
		strings.TrimSpace(sess.proxyURL) == strings.TrimSpace(proxyURL)
}

func codexWebsocketRequestUsesSIWC(ctx context.Context, auth *cliproxyauth.Auth, headers http.Header, explicitUseSIWC ...bool) bool {
	if len(explicitUseSIWC) > 0 {
		return explicitUseSIWC[0]
	}
	if !codexUseSIWC(auth) {
		return false
	}
	if headers != nil {
		if authHeader := strings.TrimSpace(headers.Get("Authorization")); strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
			tok := strings.TrimSpace(authHeader[len("Bearer "):])
			if siwcTok := codexSIWCAccessToken(auth); siwcTok != "" && tok != "" {
				return tok == siwcTok
			}
		}
	}
	return codexUseSIWCForRequest(ctx, auth, headers)
}

func detachMismatchedWebsocketSessionConn(sess *codexWebsocketSession, authID string, wsURL string, proxyURL string, useSIWC ...bool) (*websocket.Conn, *websocketConnectionCloser, string, string, cliproxyexecutor.ExecutionLifecycle) {
	if sess == nil {
		return nil, nil, "", "", nil
	}

	fence := sess.lifecycleFence()
	fence.Lock()
	defer fence.Unlock()
	sess.connMu.Lock()
	defer sess.connMu.Unlock()
	conn := sess.conn
	if conn == nil || websocketSessionTargetMatches(sess, authID, wsURL, proxyURL, useSIWC...) {
		return nil, nil, "", "", nil
	}

	previousAuthID := sess.authID
	previousWSURL := sess.wsURL
	lifecycle := sess.lifecycle
	closer := sess.connCloser
	sess.lifecycle = nil
	sess.lifecycleModel = ""
	sess.conn = nil
	sess.connCloser = nil
	sess.connCfg = nil
	sess.hasConnCfg = false
	sess.useSIWC = false
	sess.multiAgentV2OptimizedConn = nil
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	return conn, closer, previousAuthID, previousWSURL, lifecycle
}

func configureRawCodexWebsocketConn(conn *websocket.Conn, authID string, wsURL string) {
	if conn == nil {
		return
	}
	conn.SetPingHandler(func(appData string) error {
		log.Debugf("codex websockets: upstream ping received session= session_object=none ping_bytes=%d", len(appData))
		log.Debugf("codex websockets: upstream pong write started session= session_object=none")
		start := time.Now()
		errPong := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(10*time.Second))
		if errPong != nil {
			log.Warnf("codex websockets: upstream pong write failed session= session_object=none duration=%v err=%v", time.Since(start), errPong)
		} else {
			log.Debugf("codex websockets: upstream pong replied session= session_object=none duration=%v", time.Since(start))
		}
		return errPong
	})
}

func (s *codexWebsocketSession) setLastEventType(conn *websocket.Conn, eventType string) {
	if s == nil || conn == nil || eventType == "" {
		return
	}
	s.lastEventMu.Lock()
	if s.lastEventConn == conn {
		s.lastEventType = eventType
	}
	s.lastEventMu.Unlock()
}

func (s *codexWebsocketSession) getLastEventType(conn *websocket.Conn) string {
	if s == nil || conn == nil {
		return ""
	}
	s.lastEventMu.Lock()
	defer s.lastEventMu.Unlock()
	if s.lastEventConn != conn {
		return ""
	}
	return s.lastEventType
}

func newEphemeralCodexWebsocketSession() *codexWebsocketSession {
	return &codexWebsocketSession{
		sessionID:            "",
		upstreamDisconnectCh: make(chan error, 1),
	}
}

func (s *codexWebsocketSession) resetUpstreamDisconnectError(conn *websocket.Conn) {
	if s == nil || conn == nil {
		return
	}
	s.upstreamDisconnectErrMu.Lock()
	s.upstreamDisconnectErrConn = conn
	s.upstreamDisconnectErr = nil
	s.upstreamDisconnectErrMu.Unlock()

	s.lastEventMu.Lock()
	s.lastEventConn = conn
	s.lastEventType = ""
	s.lastEventMu.Unlock()
}

func (s *codexWebsocketSession) setUpstreamDisconnectError(conn *websocket.Conn, err error) {
	if s == nil || conn == nil || err == nil {
		return
	}
	s.upstreamDisconnectErrMu.Lock()
	if s.upstreamDisconnectErrConn == conn && s.upstreamDisconnectErr == nil {
		s.upstreamDisconnectErr = err
	}
	s.upstreamDisconnectErrMu.Unlock()
}

func (s *codexWebsocketSession) upstreamDisconnectError(conn *websocket.Conn) error {
	if s == nil || conn == nil {
		return nil
	}
	s.upstreamDisconnectErrMu.RLock()
	defer s.upstreamDisconnectErrMu.RUnlock()
	if s.upstreamDisconnectErrConn != conn {
		return nil
	}
	return s.upstreamDisconnectErr
}

// A logical subscriber belongs to the selected physical connection generation.
// Holding the owner lock through publication prevents a late old-connection
// failure from being delivered after a replacement has become active.
func (s *codexWebsocketSession) notifyUpstreamDisconnect(err error, generation ...*websocket.Conn) {
	if s == nil {
		return
	}
	if owner := s.disconnectOwner; owner != nil {
		if len(generation) != 1 || generation[0] == nil {
			return
		}
		owner.connMu.Lock()
		defer owner.connMu.Unlock()
		if owner.closed || owner.activeDisconnectSession != s || owner.activeDisconnectConn != generation[0] {
			return
		}
		owner.notifyUpstreamDisconnect(err)
	}
	s.upstreamDisconnectOnce.Do(func() {
		if s.upstreamDisconnectCh == nil {
			return
		}
		select {
		case s.upstreamDisconnectCh <- err:
		default:
		}
		close(s.upstreamDisconnectCh)
	})
}

func (s *codexWebsocketSession) bindDisconnectOwner(conn *websocket.Conn) {
	if s == nil || s.disconnectOwner == nil || conn == nil {
		return
	}
	fence := s.lifecycleFence()
	fence.Lock()
	defer fence.Unlock()
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.closed || s.conn != conn {
		return
	}
	owner := s.disconnectOwner
	owner.connMu.Lock()
	defer owner.connMu.Unlock()
	if owner.closed {
		return
	}
	owner.activeDisconnectSession = s
	owner.activeDisconnectConn = conn
}

func executionSessionIDFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

// getOrCreateSession resolves (or creates) the websocket session for sessionID.
// keyArgs optionally carries the fork's per-account isolation key material
// (auth *cliproxyauth.Auth, wsURL string); when present the session is stored
// under the composite store key so different accounts (or proxy/TLS profiles)
// sharing the same execution sessionID never collide onto one upstream socket.
// Without key material this returns a logical notification anchor, held in a
// separate index. Only composite entries consume physical connection/lease state.
func (e *CodexWebsocketsExecutor) getOrCreateSession(sessionID string, keyArgs ...any) *codexWebsocketSession {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	if e == nil {
		return nil
	}
	var auth *cliproxyauth.Auth
	var wsURL string
	hasIsolationKey := false
	if len(keyArgs) > 0 {
		if a, ok := keyArgs[0].(*cliproxyauth.Auth); ok {
			auth = a
		}
		hasIsolationKey = true
	}
	if len(keyArgs) > 1 {
		if u, ok := keyArgs[1].(string); ok {
			wsURL = u
		}
	}
	store := e.store
	if store == nil {
		store = globalCodexWebsocketSessionStore
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.sessions == nil {
		store.sessions = make(map[string]*codexWebsocketSession)
	}
	if store.disconnects == nil {
		store.disconnects = make(map[string]*codexWebsocketSession)
	}
	owner := store.disconnects[sessionID]
	if owner == nil {
		owner = newCodexWebsocketSession(sessionID, "", "")
		store.disconnects[sessionID] = owner
	}
	if !hasIsolationKey {
		return owner
	}
	storeKey := sessionID
	if hasIsolationKey {
		proxyURL := codexWebsocketEffectiveProxyURL(e.cfg, auth)
		if len(keyArgs) > 2 {
			if override, ok := keyArgs[2].(string); ok {
				proxyURL = override
			}
		}
		storeKey = codexWebsocketSessionStoreKey(sessionID, auth, wsURL, proxyURL)
	}
	if sess, ok := store.sessions[storeKey]; ok && sess != nil {
		return sess
	}
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	sess := newCodexWebsocketSession(sessionID, authID, wsURL)
	sess.disconnectOwner = owner
	store.sessions[storeKey] = sess
	return sess
}

func (e *CodexWebsocketsExecutor) UpstreamDisconnectChan(sessionID string) <-chan error {
	sess := e.getOrCreateSession(sessionID)
	if sess == nil {
		return nil
	}
	return sess.upstreamDisconnectCh
}

func (e *CodexWebsocketsExecutor) ensureUpstreamConn(ctx context.Context, auth *cliproxyauth.Auth, sess *codexWebsocketSession, authID string, wsURL string, headers http.Header) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error) {
	return e.ensureUpstreamConnWithSIWC(ctx, auth, sess, authID, wsURL, headers, codexWebsocketRequestUsesSIWC(ctx, auth, headers))
}

func (e *CodexWebsocketsExecutor) ensureUpstreamConnWithSIWC(ctx context.Context, auth *cliproxyauth.Auth, sess *codexWebsocketSession, authID string, wsURL string, headers http.Header, useSIWC bool) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error) {
	if sess == nil {
		conn, closer, resp, err := e.dialCodexWebsocket(ctx, auth, wsURL, headers)
		if conn != nil {
			configureRawCodexWebsocketConn(conn, authID, wsURL)
		}
		return conn, closer, resp, err
	}
	proxyURL := executionProxyURL(ctx, sess.effectiveConfig(authID, e.cfg), auth)
	if staleConn, staleCloser, staleAuthID, staleWSURL, staleLifecycle := detachMismatchedWebsocketSessionConn(sess, authID, wsURL, proxyURL, useSIWC); staleConn != nil {
		staleLastEvent := sess.getLastEventType(staleConn)
		logCodexWebsocketDisconnectedWithLastEvent(sess, sess.sessionID, staleAuthID, staleWSURL, "target_changed", staleLastEvent, nil)
		if staleCloser != nil {
			if errClose := staleCloser.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close stale websocket error: %v", errClose)
			}
		}
		if staleLifecycle != nil {
			staleLifecycle.End("target_changed")
		}
	}

	sess.connMu.Lock()
	if sess.closed {
		sess.connMu.Unlock()
		return nil, nil, nil, errCodexWebsocketSessionClosed
	}
	conn := sess.conn
	closer := sess.connCloser
	readerConn := sess.readerConn
	closedContext := sess.closedContext
	sess.connMu.Unlock()
	if conn != nil {
		sess.bindDisconnectOwner(conn)
		if readerConn != conn {
			sess.connMu.Lock()
			sess.readerConn = conn
			sess.connMu.Unlock()
			sess.configureConn(conn)
			go e.readUpstreamLoop(sess, conn)
		}
		logCodexWebsocketConnectedWithReused(sess, sess.sessionID, authID, wsURL, true)
		return conn, closer, nil, nil
	}

	dialProxyURL := executionProxyURL(ctx, e.cfg, auth)
	dialCtx, cancelDial := context.WithCancel(ctx)
	defer cancelDial()
	if closedContext != nil {
		stopClose := context.AfterFunc(closedContext, cancelDial)
		defer stopClose()
	}
	conn, closer, resp, errDial := e.dialCodexWebsocket(dialCtx, auth, wsURL, headers)
	if errDial != nil {
		return nil, closer, resp, errDial
	}

	fence := sess.lifecycleFence()
	fence.Lock()
	sess.connMu.Lock()
	if sess.closed {
		sess.connMu.Unlock()
		fence.Unlock()
		_ = closer.Close()
		return nil, nil, resp, errCodexWebsocketSessionClosed
	}
	if sess.conn != nil && websocketSessionTargetMatches(sess, authID, wsURL, dialProxyURL, useSIWC) {
		previous := sess.conn
		previousCloser := sess.connCloser
		sess.connMu.Unlock()
		fence.Unlock()
		if errClose := closer.Close(); errClose != nil {
			log.Errorf("codex websockets executor: close websocket error: %v", errClose)
		}
		sess.bindDisconnectOwner(previous)
		logCodexWebsocketConnectedWithReused(sess, sess.sessionID, authID, wsURL, true)
		return previous, previousCloser, nil, nil
	}
	sess.conn = conn
	sess.connCloser = closer
	sess.connEpoch, sess.connProvider, sess.connAuthProxy = 0, "", ""
	if auth != nil {
		sess.connEpoch = auth.RegistrationEpoch
		sess.connProvider = auth.Provider
		sess.connAuthProxy = strings.TrimSpace(auth.ProxyURL)
	}
	sess.connRequestProxy = cliproxyexecutor.RequestProxyURL(ctx)
	sess.connCfg = e.cfg
	sess.hasConnCfg = true
	sess.multiAgentV2OptimizedConn = nil
	sess.wsURL = wsURL
	sess.authID = authID
	sess.proxyURL = dialProxyURL
	sess.useSIWC = useSIWC
	sess.readerConn = conn
	sess.connMu.Unlock()
	fence.Unlock()

	sess.bindDisconnectOwner(conn)
	sess.configureConn(conn)
	go e.readUpstreamLoop(sess, conn)
	logCodexWebsocketConnectedWithReused(sess, sess.sessionID, authID, wsURL, false)
	return conn, closer, resp, nil
}

func (e *CodexWebsocketsExecutor) readUpstreamLoop(sess *codexWebsocketSession, conn *websocket.Conn) {
	if e == nil || sess == nil || conn == nil {
		return
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		readStarted := time.Now()
		msgType, payload, errRead := conn.ReadMessage()
		arrival := time.Now()
		readDuration := arrival.Sub(readStarted)
		if errRead != nil {
			sess.markTerminalError(conn, errRead)
			invalidate := func() {
				e.invalidateUpstreamConn(sess, conn, "upstream_disconnected", errRead)
			}
			invalidated := false
			ch, done := sess.activeForConn(conn)
			if ch != nil {
				invalidated = sendTerminalWebsocketRead(ch, done, codexWebsocketRead{conn: conn, err: errRead, arrival: arrival, readDuration: readDuration}, invalidate)
				if sess.clearActive(conn, ch) {
					close(ch)
				}
			}
			if !invalidated {
				invalidate()
			}
			return
		}

		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				errBinary := fmt.Errorf("codex websockets executor: unexpected binary message")
				sess.markTerminalError(conn, errBinary)
				invalidate := func() {
					e.invalidateUpstreamConn(sess, conn, "unexpected_binary", errBinary)
				}
				invalidated := false
				ch, done := sess.activeForConn(conn)
				if ch != nil {
					invalidated = sendTerminalWebsocketRead(ch, done, codexWebsocketRead{conn: conn, err: errBinary, arrival: arrival, readDuration: readDuration}, invalidate)
					if sess.clearActive(conn, ch) {
						close(ch)
					}
				}
				if !invalidated {
					invalidate()
				}
				return
			}
			continue
		}

		payload = bytes.TrimSpace(payload)
		if len(payload) > 0 {
			eventType := gjson.GetBytes(payload, "type").String()
			if eventType != "" {
				sess.setLastEventType(conn, eventType)
			}
			// Record terminal IDs before delivery: downstream may still be
			// processing this turn after its active read channel is cleared.
			responseID := gjson.GetBytes(payload, "response.id")
			if isTerminalEvent(eventType) && eventType != "error" && responseID.Type == gjson.String && responseID.String() != "" {
				sess.activeMu.Lock()
				if sess.responseStateConn == conn {
					sess.terminalResponseIDs[responseID.String()] = struct{}{}
				}
				sess.activeMu.Unlock()
			}
		}

		ch, done := sess.activeForConn(conn)
		if ch == nil {
			continue
		}
		select {
		case ch <- codexWebsocketRead{conn: conn, msgType: msgType, payload: payload, arrival: arrival, readDuration: readDuration}:
		case <-done:
		}
	}
}

func (e *CodexWebsocketsExecutor) invalidateUpstreamConn(sess *codexWebsocketSession, conn *websocket.Conn, reason string, err error) {
	e.invalidateUpstreamConnWithNotify(sess, conn, reason, err, true)
}

func (e *CodexWebsocketsExecutor) invalidateUpstreamConnWithoutDisconnectNotify(sess *codexWebsocketSession, conn *websocket.Conn, reason string, err error) {
	e.invalidateUpstreamConnWithNotify(sess, conn, reason, err, false)
}

func (e *CodexWebsocketsExecutor) invalidateUpstreamConnWithNotify(sess *codexWebsocketSession, conn *websocket.Conn, reason string, err error, notify bool) {
	if sess == nil || conn == nil {
		return
	}

	fence := sess.lifecycleFence()
	fence.Lock()
	sess.connMu.Lock()
	current := sess.conn
	authID := sess.authID
	wsURL := sess.wsURL
	sessionID := sess.sessionID
	if current == nil || current != conn {
		sess.connMu.Unlock()
		fence.Unlock()
		return
	}
	lifecycle := sess.lifecycle
	closer := sess.connCloser
	sess.lifecycle = nil
	sess.lifecycleModel = ""
	sess.conn = nil
	sess.connCloser = nil
	sess.connCfg = nil
	sess.hasConnCfg = false
	sess.multiAgentV2OptimizedConn = nil
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	sess.connMu.Unlock()
	fence.Unlock()

	lastEvent := sess.getLastEventType(conn)
	logCodexWebsocketDisconnectedWithLastEvent(sess, sessionID, authID, wsURL, reason, lastEvent, err)
	if notify {
		sess.notifyUpstreamDisconnect(err, conn)
	}
	if closer != nil {
		if errClose := closer.Close(); errClose != nil {
			log.Errorf("codex websockets executor: close websocket error: %v", errClose)
		}
	}
	if lifecycle != nil {
		lifecycle.End(reason)
	}
}

func (e *CodexWebsocketsExecutor) sessionStore() *codexWebsocketSessionStore {
	if e == nil {
		return nil
	}
	if e.store != nil {
		return e.store
	}
	return globalCodexWebsocketSessionStore
}

func (e *CodexWebsocketsExecutor) SharesExecutionSessionsWith(other cliproxyauth.ProviderExecutor) bool {
	if e == nil || other == nil {
		return false
	}
	store := e.sessionStore()
	if store == nil {
		return false
	}
	switch next := other.(type) {
	case *CodexWebsocketsExecutor:
		return next != nil && store == next.sessionStore()
	case *CodexAutoExecutor:
		return next != nil && next.wsExec != nil && store == next.wsExec.sessionStore()
	default:
		return false
	}
}

func (e *CodexWebsocketsExecutor) CloseAuthExecutionSessions(authID string) {
	if e == nil {
		return
	}
	closeCodexWebsocketSessionsInStoreForAuthID(e.sessionStore(), authID, "auth_removed")
}

func (e *CodexWebsocketsExecutor) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if e == nil || sessionID == "" {
		return
	}
	if sessionID == cliproxyauth.CloseAllExecutionSessionsID {
		// Manager 的 SharesExecutionSessionsWith 保留同 store 热更；显式关闭仍释放连接。
		e.closeAllExecutionSessions("executor_shutdown")
		return
	}
	closeCodexWebsocketLogicalSessions(e.sessionStore(), sessionID, "session_closed", "")
}

func (e *CodexWebsocketsExecutor) closeAllExecutionSessions(reason string) {
	if e == nil {
		return
	}
	store := e.sessionStore()
	ids := make(map[string]struct{})
	store.mu.Lock()
	for _, sess := range store.sessions {
		if sess != nil {
			ids[sess.sessionID] = struct{}{}
		}
	}
	for id := range store.disconnects {
		ids[id] = struct{}{}
	}
	store.mu.Unlock()
	for id := range ids {
		closeCodexWebsocketLogicalSessions(store, id, reason, "")
	}
}

func (e *CodexWebsocketsExecutor) closeExecutionSession(sess *codexWebsocketSession, reason string) {
	closeCodexWebsocketSession(sess, reason)
}

func closeCodexWebsocketSession(sess *codexWebsocketSession, reason string) {
	if sess == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "session_closed"
	}

	fence := sess.lifecycleFence()
	fence.Lock()
	sess.connMu.Lock()
	sess.closed = true
	if sess.closeCancel != nil {
		sess.closeCancel()
	}
	conn := sess.conn
	authID := sess.authID
	wsURL := sess.wsURL
	lifecycle := sess.lifecycle
	closer := sess.connCloser
	sess.lifecycle = nil
	sess.lifecycleModel = ""
	sess.conn = nil
	sess.connCloser = nil
	sess.connCfg = nil
	sess.hasConnCfg = false
	sess.multiAgentV2OptimizedConn = nil
	if sess.readerConn == conn {
		sess.readerConn = nil
	}
	sessionID := sess.sessionID
	sess.connMu.Unlock()
	fence.Unlock()

	lastEvent := sess.getLastEventType(conn)
	if conn != nil {
		logCodexWebsocketDisconnectedWithLastEvent(sess, sessionID, authID, wsURL, reason, lastEvent, nil)
		if closer != nil {
			if errClose := closer.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
		}
	}
	if lifecycle != nil {
		lifecycle.End(reason)
	}
}

// codexWebsocketSessionStoreKey builds the fork's per-account isolation key for
// the session store: sessionID | auth identity | proxy URL | wsURL | TLS/transport
// profile token. Keeping every component means two different accounts (or the same
// account behind different proxy/TLS profiles) that happen to share an execution
// sessionID never collapse onto one upstream websocket (cross-account correlation
// + wrong-account routing). Taking upstream's bare-sessionID key would reintroduce
// that leak.
func codexWebsocketSessionStoreKey(sessionID string, auth *cliproxyauth.Auth, wsURL string, proxyURLOverride ...string) string {
	baseSessionID := strings.TrimSpace(sessionID)
	if baseSessionID == "" {
		return ""
	}
	proxyURL := ""
	if len(proxyURLOverride) > 0 {
		proxyURL = strings.TrimSpace(proxyURLOverride[0])
	} else if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	epoch := uint64(0)
	if auth != nil {
		epoch = auth.RegistrationEpoch
	}
	return fmt.Sprintf(
		"%s|%s|%s|%s|%s|epoch:%d",
		baseSessionID,
		codexWebsocketAuthKey(auth),
		proxyURL,
		strings.TrimSpace(wsURL),
		codexWebsocketTransportProfileToken(auth),
		epoch,
	)
}

func codexWebsocketAuthKey(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return "anonymous"
	}
	for _, value := range []string{auth.ID, auth.FileName, auth.Label} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return "anonymous"
}

func codexWebsocketTransportProfileToken(auth *cliproxyauth.Auth) string {
	return helps.RuntimeTransportProfileToken(auth)
}

func codexWebsocketEffectiveProxyURL(cfg *config.Config, auth *cliproxyauth.Auth) string {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.ProxyURL)
	}
	return ""
}

func sessionObjectKind(sess *codexWebsocketSession) string {
	if sess == nil {
		return "none"
	}
	if strings.TrimSpace(sess.sessionID) != "" {
		return "persistent"
	}
	return "ephemeral"
}

func logCodexWebsocketConnected(sessionID string, authID string, wsURL string) {
	logCodexWebsocketConnectedWithReused(nil, sessionID, authID, wsURL, false)
}

func logCodexWebsocketConnectedWithReused(sess *codexWebsocketSession, sessionID string, authID string, wsURL string, reused bool) {
	sessionStr := strings.TrimSpace(sessionID)
	sessionKind := sessionObjectKind(sess)
	if reused {
		log.Infof("codex websockets: upstream connected session=%s auth=%s url=%s session_object=%s reused=true", sessionStr, strings.TrimSpace(authID), strings.TrimSpace(wsURL), sessionKind)
		return
	}
	log.Infof("codex websockets: upstream connected session=%s auth=%s url=%s session_object=%s reused=false", sessionStr, strings.TrimSpace(authID), strings.TrimSpace(wsURL), sessionKind)
}

func logCodexWebsocketDisconnected(sessionID string, authID string, wsURL string, reason string, err error) {
	logCodexWebsocketDisconnectedWithLastEvent(nil, sessionID, authID, wsURL, reason, "", err)
}

func isTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.done", "response.incomplete", "response.failed", "error":
		return true
	default:
		return false
	}
}

func logCodexWebsocketDisconnectedWithLastEvent(sess *codexWebsocketSession, sessionID string, authID string, wsURL string, reason string, lastEvent string, err error) {
	sessionStr := strings.TrimSpace(sessionID)
	sessionKind := sessionObjectKind(sess)
	terminalStatus := isTerminalEvent(lastEvent)
	if err != nil {
		if lastEvent != "" {
			log.Infof("codex websockets: upstream disconnected session=%s auth=%s url=%s session_object=%s reason=%s last_event=%s is_terminal=%t err=%v", sessionStr, strings.TrimSpace(authID), strings.TrimSpace(wsURL), sessionKind, strings.TrimSpace(reason), lastEvent, terminalStatus, err)
			return
		}
		log.Infof("codex websockets: upstream disconnected session=%s auth=%s url=%s session_object=%s reason=%s is_terminal=false err=%v", sessionStr, strings.TrimSpace(authID), strings.TrimSpace(wsURL), sessionKind, strings.TrimSpace(reason), err)
		return
	}
	if lastEvent != "" {
		log.Infof("codex websockets: upstream disconnected session=%s auth=%s url=%s session_object=%s reason=%s last_event=%s is_terminal=%t", sessionStr, strings.TrimSpace(authID), strings.TrimSpace(wsURL), sessionKind, strings.TrimSpace(reason), lastEvent, terminalStatus)
		return
	}
	log.Infof("codex websockets: upstream disconnected session=%s auth=%s url=%s session_object=%s reason=%s is_terminal=false", sessionStr, strings.TrimSpace(authID), strings.TrimSpace(wsURL), sessionKind, strings.TrimSpace(reason))
}

// CloseCodexWebsocketSessionsForAuthID closes all active Codex upstream websocket sessions
// associated with the supplied auth ID.
func CloseCodexWebsocketSessionsForAuthID(authID string, reason string) {
	closeCodexWebsocketSessionsInStoreForAuthID(globalCodexWebsocketSessionStore, authID, reason)
}

func closeCodexWebsocketSessionsInStoreForAuthID(store *codexWebsocketSessionStore, authID string, reason string) {
	authID = strings.TrimSpace(authID)
	if authID == "" || store == nil {
		return
	}
	if strings.TrimSpace(reason) == "" {
		reason = "auth_removed"
	}
	store.mu.Lock()
	sessions := make([]*codexWebsocketSession, 0, len(store.sessions))
	for _, sess := range store.sessions {
		if sess != nil {
			sessions = append(sessions, sess)
		}
	}
	store.mu.Unlock()
	// 先筛账号，避免在无关逻辑会话的 wire fence 上等待；进入 fence 后仍复核。
	ids := make(map[string]struct{})
	for _, sess := range sessions {
		sess.connMu.Lock()
		matches := strings.TrimSpace(sess.authID) == authID
		sess.connMu.Unlock()
		if matches {
			ids[sess.sessionID] = struct{}{}
		}
	}
	for id := range ids {
		closeCodexWebsocketLogicalSessions(store, id, reason, authID)
	}
}

// 先拿逻辑 generation fence，再删除索引。这样同 ID 重开只能在旧控制帧提交完后
// 创建新 owner；任何等待 fence、socket.Close 或 lifecycle.End 都不持 store/状态锁。
func closeCodexWebsocketLogicalSessions(store *codexWebsocketSessionStore, id, reason, authID string) {
	if store == nil {
		return
	}
	for {
		store.mu.Lock()
		owner := store.disconnects[id]
		store.mu.Unlock()
		if owner != nil {
			owner.interruptFence.Lock()
		}
		store.mu.Lock()
		if store.disconnects[id] != owner {
			store.mu.Unlock()
			if owner != nil {
				owner.interruptFence.Unlock()
			}
			continue
		}
		items := make(map[string]*codexWebsocketSession)
		for key, sess := range store.sessions {
			if sess != nil && sess.sessionID == id {
				items[key] = sess
			}
		}
		store.mu.Unlock()
		matches := make(map[string]*codexWebsocketSession)
		for key, sess := range items {
			sess.connMu.Lock()
			selected := authID == "" || strings.TrimSpace(sess.authID) == authID
			sess.connMu.Unlock()
			if selected {
				matches[key] = sess
			}
		}
		store.mu.Lock()
		if store.disconnects[id] != owner {
			store.mu.Unlock()
			if owner != nil {
				owner.interruptFence.Unlock()
			}
			continue
		}
		closing := make([]*codexWebsocketSession, 0, len(matches)+1)
		for key, sess := range matches {
			if store.sessions[key] == sess {
				delete(store.sessions, key)
				closing = append(closing, sess)
			}
		}
		live := false
		for _, sess := range store.sessions {
			if sess != nil && sess.sessionID == id {
				live = true
				break
			}
		}
		if owner != nil && !live && (authID == "" || len(closing) > 0) {
			owner.connMu.Lock()
			owner.closed = true
			owner.connMu.Unlock()
			delete(store.disconnects, id)
			closing = append(closing, owner)
		}
		if len(closing) > 0 {
			store.notifyLeaseChangedLocked()
		}
		store.mu.Unlock()
		if owner != nil {
			owner.interruptFence.Unlock()
		}
		for _, sess := range closing {
			closeCodexWebsocketSession(sess, reason)
		}
		return
	}
}
