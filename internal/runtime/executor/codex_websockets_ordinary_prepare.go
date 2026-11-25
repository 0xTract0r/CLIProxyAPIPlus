package executor

import (
	"context"
	"net/http"
	"strings"
	"unicode"

	"github.com/MoeForge/moego/ojson"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type codexWebsocketPrepared struct {
	from                     sdktranslator.Format
	responseFormat           sdktranslator.Format
	to                       sdktranslator.Format
	preserveNativeOutput     bool
	originalPayload          []byte
	clientBody               []byte
	upstreamModel            string
	wsURL                    string
	wsHeaders                http.Header
	replayScope              codexReasoningReplayScope
	optimizeMultiAgentV2     bool
	multiAgentV2Conflict     bool
	sessionInstructions      helps.CodexInstructionTurn
	sessionReceipt           helps.CodexInstructionReceipt
	unconfiguredInstructions string
}

func validOrdinaryCodexRequestID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if r > unicode.MaxASCII || !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._:-", r)) {
			return false
		}
	}
	return true
}

func (e *CodexWebsocketsExecutor) prepareCodexWebsocketStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, sessions ...*codexWebsocketSession) (*codexWebsocketPrepared, error) {
	if err := rejectSyntheticCompactionPreviousResponseID(ctx, req.Payload); err != nil {
		return nil, err
	}
	body, err := e.prepareOrdinaryCodexBody(ctx, auth, req, opts, true, e.resolveCodexModelIsCompat(auth, req, req.Model))
	if err != nil {
		return nil, err
	}
	doc := ojson.NewDoc(body)
	input := ojson.NewDoc(req.Payload)
	if event := input.Get("event_id").String(); validOrdinaryCodexRequestID(event) {
		_ = doc.Set("event_id", event)
	}
	if previous := input.Get("previous_response_id").String(); validOrdinaryCodexRequestID(previous) {
		_ = doc.Set("previous_response_id", previous)
	}
	if typ := input.Get("type").String(); typ == "response.append" {
		_ = doc.Set("type", typ)
	}
	unconfigured := strings.Clone(input.Get("instructions").Raw)
	var chain helps.CodexInstructionChain
	var state = &chain
	var conn *websocket.Conn
	if len(sessions) > 0 && sessions[0] != nil {
		sess := sessions[0]
		state = &sess.sessionInstructions
		sess.connMu.Lock()
		conn = sess.conn
		sess.connMu.Unlock()
	}
	instructionTurn, err := helps.PrepareCodexSessionInstructions(ctx, doc, req, opts, state, conn, false)
	if err != nil {
		return nil, err
	}
	if err = instructionTurn.CheckFinal(doc.Bytes()); err != nil {
		return nil, err
	}

	_, base := ordinaryCodexImageCredential(auth)
	origin, err := ordinaryCodexOrigin(base)
	if err != nil {
		return nil, err
	}
	origin.Path = strings.TrimRight(origin.Path, "/") + "/responses"
	origin.RawPath = ""
	upstream, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.String(), nil)
	if err != nil {
		return nil, err
	}
	if err = e.PrepareRequest(upstream, auth); err != nil {
		return nil, err
	}
	e.applyOrdinaryCodexModelHeaders(upstream, auth, req.Model)
	if key := opts.Headers.Get("X-Client-Request-Id"); validOrdinaryCodexRequestID(key) {
		upstream.Header.Set("X-Client-Request-Id", key)
	}
	wsURL, err := buildCodexResponsesWebsocketURL(upstream.URL.String())
	if err != nil {
		return nil, err
	}
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	return &codexWebsocketPrepared{from: opts.SourceFormat, responseFormat: cliproxyexecutor.ResponseFormatOrSource(opts), to: sdktranslator.FromString("codex"), preserveNativeOutput: opts.SourceFormat == sdktranslator.FormatCodex, originalPayload: original,
		clientBody: doc.Bytes(), upstreamModel: doc.Get("model").String(), wsURL: wsURL, wsHeaders: upstream.Header, sessionInstructions: instructionTurn, unconfiguredInstructions: unconfigured}, nil
}
