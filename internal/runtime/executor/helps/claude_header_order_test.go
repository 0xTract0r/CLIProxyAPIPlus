package helps

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// headerNamesFromWire parses a raw HTTP/1.1 request byte stream and returns the
// header names in exact wire order (original casing preserved). It deliberately
// does NOT use http.ReadRequest / textproto, because those parse headers into a
// map and lose both order and casing — the very things under test.
func headerNamesFromWire(t *testing.T, raw []byte) (reqLine string, names []string) {
	t.Helper()
	idx := bytes.Index(raw, []byte("\r\n\r\n"))
	if idx < 0 {
		t.Fatalf("no header terminator in wire bytes:\n%q", raw)
	}
	head := string(raw[:idx])
	lines := strings.Split(head, "\r\n")
	reqLine = lines[0]
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			names = append(names, line)
			continue
		}
		names = append(names, line[:colon])
	}
	return reqLine, names
}

func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("header count mismatch:\n got (%d) = %v\nwant (%d) = %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("header[%d] = %q, want %q\nfull got  = %v\nfull want = %v", i, got[i], want[i], got, want)
		}
	}
}

// captureWireBytes runs req.Write over writer (usually a wrapped conn backed by
// one end of net.Pipe) and returns everything read from the peer end.
func captureWireBytes(t *testing.T, req *http.Request, makeWriter func(inner net.Conn) net.Conn) []byte {
	t.Helper()
	c1, c2 := net.Pipe()
	writer := makeWriter(c1)

	done := make(chan error, 1)
	go func() {
		werr := req.Write(writer)
		_ = c1.Close()
		done <- werr
	}()

	raw, rerr := io.ReadAll(c2)
	if rerr != nil {
		t.Fatalf("read wire bytes: %v", rerr)
	}
	if werr := <-done; werr != nil {
		t.Fatalf("req.Write: %v", werr)
	}
	return raw
}

func newClaudeOAuthRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// Set headers the way applyClaudeHeaders does (canonical Set), OAuth/Bearer
	// mode. Casing here is Go-canonical; the wrapper is responsible for restoring
	// the real claude-cli wire casing.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer dummy-not-a-real-bearer")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.220 (external, cli)")
	req.Header.Set("X-Claude-Code-Session-Id", "00000000-0000-0000-0000-000000000000")
	req.Header.Set("X-Stainless-Arch", "arm64")
	req.Header.Set("X-Stainless-Lang", "js")
	req.Header.Set("X-Stainless-OS", "MacOS")
	req.Header.Set("X-Stainless-Package-Version", "0.94.0")
	req.Header.Set("X-Stainless-Retry-Count", "0")
	req.Header.Set("X-Stainless-Runtime", "node")
	req.Header.Set("X-Stainless-Runtime-Version", "v24.3.0")
	req.Header.Set("X-Stainless-Timeout", "600")
	req.Header.Set("Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14")
	req.Header.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("X-App", "cli")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	return req
}

// claudeOAuthWireOrder is the exact target order + casing captured zero-account
// from real claude-cli 2.1.220 in OAuth/Bearer mode
// (docs/fingerprint/cpa-reqs/phase3-evidence/header-order-probe/COMPARISON-oauth.txt).
var claudeOAuthWireOrder = []string{
	"Accept",
	"Authorization",
	"Content-Type",
	"User-Agent",
	"X-Claude-Code-Session-Id",
	"X-Stainless-Arch",
	"X-Stainless-Lang",
	"X-Stainless-OS",
	"X-Stainless-Package-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"X-Stainless-Timeout",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-app",
	"Connection",
	"Host",
	"Accept-Encoding",
	"Content-Length",
}

// TestClaudeHeaderOrderConn_WireOrderMatchesRealClaudeCLI is the core assertion:
// stdlib serializes the claude request in canonical Title-Case + alphabetical
// order, and the wrapped conn must rewrite the on-wire bytes to the exact real
// claude-cli order + casing. Reads raw bytes off a net.Pipe (no map parsing).
func TestClaudeHeaderOrderConn_WireOrderMatchesRealClaudeCLI(t *testing.T) {
	req := newClaudeOAuthRequest(t, `{"model":"claude-3-5-sonnet","max_tokens":8}`)
	raw := captureWireBytes(t, req, newClaudeHeaderOrderConn)

	reqLine, names := headerNamesFromWire(t, raw)
	if reqLine != "POST /v1/messages?beta=true HTTP/1.1" {
		t.Fatalf("request line = %q", reqLine)
	}
	assertOrder(t, names, claudeOAuthWireOrder)

	// Sanity: the header terminator and a Content-Length are present, and the
	// body followed intact.
	if !bytes.Contains(raw, []byte("Content-Type: application/json\r\n")) {
		t.Fatalf("Content-Type value/casing not preserved:\n%q", raw)
	}
	if !bytes.HasSuffix(raw, []byte(`{"model":"claude-3-5-sonnet","max_tokens":8}`)) {
		t.Fatalf("request body not forwarded intact:\n%q", raw)
	}
}

// TestClaudeHeaderOrderConn_GateOffIsGoAlphabetical proves the wrapper is
// actually changing something: WITHOUT the wrapper, stdlib emits Go's canonical
// Title-Case + Host/User-Agent/Content-Length-first + alphabetical order, which
// is NOT the claude-cli order.
func TestClaudeHeaderOrderConn_GateOffIsGoAlphabetical(t *testing.T) {
	req := newClaudeOAuthRequest(t, `{"model":"claude"}`)
	raw := captureWireBytes(t, req, func(inner net.Conn) net.Conn { return inner }) // no wrap

	_, names := headerNamesFromWire(t, raw)
	wantGo := []string{
		"Host",
		"User-Agent",
		"Content-Length",
		"Accept",
		"Accept-Encoding",
		"Anthropic-Beta",
		"Anthropic-Dangerous-Direct-Browser-Access",
		"Anthropic-Version",
		"Authorization",
		"Connection",
		"Content-Type",
		"X-App",
		"X-Claude-Code-Session-Id",
		"X-Stainless-Arch",
		"X-Stainless-Lang",
		"X-Stainless-Os", // note: Go canonicalizes to "Os", not "OS"
		"X-Stainless-Package-Version",
		"X-Stainless-Retry-Count",
		"X-Stainless-Runtime",
		"X-Stainless-Runtime-Version",
		"X-Stainless-Timeout",
	}
	assertOrder(t, names, wantGo)

	// And it must DIFFER from the claude-cli target (otherwise the fix is a no-op).
	if strings.Join(names, ",") == strings.Join(claudeOAuthWireOrder, ",") {
		t.Fatal("gate-off order unexpectedly equals claude-cli order")
	}
}

// TestReorderClaudeRequestHead_APIKeyMode validates the api-key capture order
// (Authorization absent, x-api-key present between anthropic-version and x-app),
// matching docs/.../header-order-probe/COMPARISON.txt.
func TestReorderClaudeRequestHead_APIKeyMode(t *testing.T) {
	// Input in an arbitrary (Go-alphabetical-ish) order; reorder must fix it.
	in := "POST /v1/messages?beta=true HTTP/1.1\r\n" +
		"Host: api.anthropic.com\r\n" +
		"User-Agent: claude-cli/2.1.220 (external, cli)\r\n" +
		"Content-Length: 2\r\n" +
		"Accept: application/json\r\n" +
		"Accept-Encoding: gzip, deflate, br, zstd\r\n" +
		"Anthropic-Beta: claude-code-20250219\r\n" +
		"Anthropic-Dangerous-Direct-Browser-Access: true\r\n" +
		"Anthropic-Version: 2023-06-01\r\n" +
		"Connection: keep-alive\r\n" +
		"Content-Type: application/json\r\n" +
		"X-Api-Key: dummy\r\n" +
		"X-App: cli\r\n" +
		"X-Claude-Code-Session-Id: sid\r\n" +
		"X-Stainless-Arch: arm64\r\n" +
		"X-Stainless-Lang: js\r\n" +
		"X-Stainless-Os: MacOS\r\n" +
		"X-Stainless-Package-Version: 0.94.0\r\n" +
		"X-Stainless-Retry-Count: 0\r\n" +
		"X-Stainless-Runtime: node\r\n" +
		"X-Stainless-Runtime-Version: v24.3.0\r\n" +
		"X-Stainless-Timeout: 600\r\n\r\n"

	out, bodyLen, chunked := reorderClaudeRequestHead([]byte(in))
	if chunked {
		t.Fatal("unexpected chunked")
	}
	if bodyLen != 2 {
		t.Fatalf("bodyLen = %d, want 2", bodyLen)
	}
	_, names := headerNamesFromWire(t, out)
	want := []string{
		"Accept",
		"Content-Type",
		"User-Agent",
		"X-Claude-Code-Session-Id",
		"X-Stainless-Arch",
		"X-Stainless-Lang",
		"X-Stainless-OS",
		"X-Stainless-Package-Version",
		"X-Stainless-Retry-Count",
		"X-Stainless-Runtime",
		"X-Stainless-Runtime-Version",
		"X-Stainless-Timeout",
		"anthropic-beta",
		"anthropic-dangerous-direct-browser-access",
		"anthropic-version",
		"x-api-key",
		"x-app",
		"Connection",
		"Host",
		"Accept-Encoding",
		"Content-Length",
	}
	assertOrder(t, names, want)
}

// TestReorderClaudeRequestHead_UnknownBeforeTerminal ensures unknown/custom
// headers (e.g. an operator header or x-client-request-id) are emitted after the
// known application headers but before the terminal Connection/Host/... group,
// preserving their original casing and relative order.
func TestReorderClaudeRequestHead_UnknownBeforeTerminal(t *testing.T) {
	in := "POST /v1/messages HTTP/1.1\r\n" +
		"Host: api.anthropic.com\r\n" +
		"X-App: cli\r\n" +
		"X-Client-Request-Id: req-123\r\n" +
		"X-Operator-Custom: keep-me\r\n" +
		"Accept: application/json\r\n" +
		"Connection: keep-alive\r\n" +
		"Content-Length: 0\r\n\r\n"

	out, _, _ := reorderClaudeRequestHead([]byte(in))
	_, names := headerNamesFromWire(t, out)
	want := []string{
		"Accept",
		"x-app",
		// unknowns, original order + casing, before terminal group:
		"X-Client-Request-Id",
		"X-Operator-Custom",
		"Connection",
		"Host",
		"Content-Length",
	}
	assertOrder(t, names, want)
}

// TestClaudeHeaderOrderConn_KeepAliveTwoRequests verifies the wrapper correctly
// tracks the body of request #1 and reorders request #2 on a reused conn.
func TestClaudeHeaderOrderConn_KeepAliveTwoRequests(t *testing.T) {
	body1 := `{"a":1}`
	body2 := `{"bb":22}`
	req1 := "POST /v1/messages HTTP/1.1\r\n" +
		"Host: api.anthropic.com\r\n" +
		"Content-Length: " + strconv.Itoa(len(body1)) + "\r\n" +
		"Accept: application/json\r\n" +
		"X-App: cli\r\n" +
		"Connection: keep-alive\r\n\r\n" + body1
	req2 := "POST /v1/messages HTTP/1.1\r\n" +
		"Host: api.anthropic.com\r\n" +
		"Content-Length: " + strconv.Itoa(len(body2)) + "\r\n" +
		"Anthropic-Version: 2023-06-01\r\n" +
		"Accept: application/json\r\n" +
		"Connection: keep-alive\r\n\r\n" + body2

	c1, c2 := net.Pipe()
	wrapped := newClaudeHeaderOrderConn(c1)
	done := make(chan struct{})
	go func() {
		_, _ = wrapped.Write([]byte(req1))
		_, _ = wrapped.Write([]byte(req2))
		_ = c1.Close()
		close(done)
	}()
	raw, _ := io.ReadAll(c2)
	<-done

	// Split the two requests apart on the body boundary and check each order.
	parts := bytes.SplitN(raw, []byte(body1), 2)
	if len(parts) != 2 {
		t.Fatalf("could not locate body1 boundary:\n%q", raw)
	}
	_, names1 := headerNamesFromWire(t, append(parts[0], []byte("\r\n\r\n")...))
	assertOrder(t, names1, []string{"Accept", "x-app", "Connection", "Host", "Content-Length"})

	_, names2 := headerNamesFromWire(t, parts[1])
	assertOrder(t, names2, []string{"Accept", "anthropic-version", "Connection", "Host", "Content-Length"})

	if !bytes.HasSuffix(raw, []byte(body2)) {
		t.Fatalf("request #2 body not forwarded intact:\n%q", raw)
	}
}

// TestClaudeHeaderOrderConn_LargeBodySplitWrites feeds the header block and a
// large body across many small Write calls (simulating bufio flush splitting)
// and verifies the header is still reordered and the body arrives byte-for-byte.
func TestClaudeHeaderOrderConn_LargeBodySplitWrites(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 50000)
	head := "POST /v1/messages HTTP/1.1\r\n" +
		"Host: api.anthropic.com\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
		"Accept: application/json\r\n" +
		"Connection: keep-alive\r\n\r\n"
	full := append([]byte(head), body...)

	c1, c2 := net.Pipe()
	wrapped := newClaudeHeaderOrderConn(c1)
	done := make(chan struct{})
	go func() {
		// write in 1000-byte chunks
		for off := 0; off < len(full); off += 1000 {
			end := off + 1000
			if end > len(full) {
				end = len(full)
			}
			_, _ = wrapped.Write(full[off:end])
		}
		_ = c1.Close()
		close(done)
	}()
	raw, _ := io.ReadAll(c2)
	<-done

	_, names := headerNamesFromWire(t, raw)
	assertOrder(t, names, []string{"Accept", "Connection", "Host", "Content-Length"})

	idx := bytes.Index(raw, []byte("\r\n\r\n"))
	gotBody := raw[idx+4:]
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("body corrupted: got %d bytes, want %d", len(gotBody), len(body))
	}
}

// TestClaudeHeaderOrderConn_ChunkedPassthrough ensures a chunked request body
// (which cannot be length-counted) is passed through without corruption; the
// header block is still reordered but subsequent bytes are untouched.
func TestClaudeHeaderOrderConn_ChunkedPassthrough(t *testing.T) {
	chunkBody := "5\r\nhello\r\n0\r\n\r\n"
	msg := "POST /v1/messages HTTP/1.1\r\n" +
		"Host: api.anthropic.com\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Accept: application/json\r\n" +
		"Connection: keep-alive\r\n\r\n" + chunkBody

	c1, c2 := net.Pipe()
	wrapped := newClaudeHeaderOrderConn(c1)
	done := make(chan struct{})
	go func() {
		_, _ = wrapped.Write([]byte(msg))
		_ = c1.Close()
		close(done)
	}()
	raw, _ := io.ReadAll(c2)
	<-done

	_, names := headerNamesFromWire(t, raw)
	assertOrder(t, names, []string{"Accept", "Transfer-Encoding", "Connection", "Host"})
	if !bytes.HasSuffix(raw, []byte(chunkBody)) {
		t.Fatalf("chunked body not passed through intact:\n%q", raw)
	}
}

// TestMaybeWrapClaudeHeaderOrder_GateOff verifies the round tripper only wraps
// the conn when replayClaudeHeaderOrder is set; gate-off returns the conn as-is.
func TestMaybeWrapClaudeHeaderOrder_GateOff(t *testing.T) {
	inner, peer := net.Pipe()
	defer func() { _ = inner.Close() }()
	defer func() { _ = peer.Close() }()

	rtOff := &utlsRoundTripper{}
	if got := rtOff.maybeWrapClaudeHeaderOrder(inner); got != inner {
		t.Fatal("gate-off must return the conn untouched")
	}

	rtOn := &utlsRoundTripper{replayClaudeHeaderOrder: true}
	got := rtOn.maybeWrapClaudeHeaderOrder(inner)
	if _, ok := got.(*claudeHeaderOrderConn); !ok {
		t.Fatalf("gate-on must wrap the conn, got %T", got)
	}
}

// TestNewUtlsRoundTripperForProfile_HeaderOrderClaudeOnly verifies the header
// order replay flag is honored ONLY for the claude HelloCustom profile and is
// ignored for codex, so codex egress is never re-cased to the claude order.
func TestNewUtlsRoundTripperForProfile_HeaderOrderClaudeOnly(t *testing.T) {
	claudeRT := NewUtlsRoundTripperForProfileWithHeaderOrder("", claudeCLIClientHelloProfileID, true)
	if !innerUtlsReplayFlag(t, claudeRT) {
		t.Fatal("claude profile with replay=true must set replayClaudeHeaderOrder")
	}

	codexRT := NewUtlsRoundTripperForProfileWithHeaderOrder("", codexRustlsClientHelloProfileID, true)
	if innerUtlsReplayFlag(t, codexRT) {
		t.Fatal("codex profile must NOT enable claude header-order replay")
	}

	// Default constructor never enables it.
	defRT := NewUtlsRoundTripperForProfile("", claudeCLIClientHelloProfileID)
	if innerUtlsReplayFlag(t, defRT) {
		t.Fatal("default NewUtlsRoundTripperForProfile must leave replay off")
	}
}

func innerUtlsReplayFlag(t *testing.T, rt http.RoundTripper) bool {
	t.Helper()
	fb, ok := rt.(*fallbackRoundTripper)
	if !ok {
		t.Fatalf("expected *fallbackRoundTripper, got %T", rt)
	}
	u, ok := fb.utls.(*utlsRoundTripper)
	if !ok {
		t.Fatalf("expected *utlsRoundTripper, got %T", fb.utls)
	}
	return u.replayClaudeHeaderOrder
}

// serializeClaudeRequest renders req to raw HTTP/1.1 bytes exactly as net/http's
// transport does (Request.write), giving us the precise on-wire input the
// wrapper will see. We then replay these bytes through the wrapper at chosen
// Write boundaries.
func serializeClaudeRequest(t *testing.T, req *http.Request) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := req.Write(&buf); err != nil {
		t.Fatalf("serialize request: %v", err)
	}
	return buf.Bytes()
}

// captureWrappedChunks feeds the given byte chunks to a wrapped conn in order
// (each chunk = one Write call) and returns everything read from the peer end.
func captureWrappedChunks(t *testing.T, chunks [][]byte) []byte {
	t.Helper()
	c1, c2 := net.Pipe()
	wrapped := newClaudeHeaderOrderConn(c1)
	done := make(chan struct{})
	go func() {
		for _, ch := range chunks {
			_, _ = wrapped.Write(ch)
		}
		_ = c1.Close()
		close(done)
	}()
	raw, _ := io.ReadAll(c2)
	<-done
	return raw
}

func splitAt(raw []byte, offsets ...int) [][]byte {
	prev := 0
	var out [][]byte
	for _, off := range offsets {
		if off <= prev || off >= len(raw) {
			continue
		}
		out = append(out, raw[prev:off])
		prev = off
	}
	out = append(out, raw[prev:])
	return out
}

// TestClaudeHeaderOrderConn_SplitAtTerminator feeds the request so the 4-byte
// "\r\n\r\n" header terminator straddles a Write boundary. The wrapper must still
// locate the terminator across writes and produce the claude-cli order.
func TestClaudeHeaderOrderConn_SplitAtTerminator(t *testing.T) {
	body := `{"model":"claude","max_tokens":4}`
	raw := serializeClaudeRequest(t, newClaudeOAuthRequest(t, body))
	term := bytes.Index(raw, []byte("\r\n\r\n"))
	if term < 0 {
		t.Fatal("no terminator in serialized request")
	}
	// Straddle each interior byte boundary of the 4-byte terminator.
	for _, cut := range []int{term + 1, term + 2, term + 3} {
		got := captureWrappedChunks(t, splitAt(raw, cut))
		reqLine, names := headerNamesFromWire(t, got)
		if reqLine != "POST /v1/messages?beta=true HTTP/1.1" {
			t.Fatalf("cut=%d request line = %q", cut, reqLine)
		}
		assertOrder(t, names, claudeOAuthWireOrder)
		if !bytes.HasSuffix(got, []byte(body)) {
			t.Fatalf("cut=%d body not forwarded intact", cut)
		}
	}
}

// TestClaudeHeaderOrderConn_SplitInsideHeaderLine feeds the request so a single
// header line is split across a Write boundary (mid "Authorization"). The
// wrapper accumulates across writes and still reorders + re-cases correctly.
func TestClaudeHeaderOrderConn_SplitInsideHeaderLine(t *testing.T) {
	body := `{"x":1}`
	raw := serializeClaudeRequest(t, newClaudeOAuthRequest(t, body))
	// Go serializes "Authorization" canonically; cut in the middle of the name.
	pos := bytes.Index(raw, []byte("Authorization"))
	if pos < 0 {
		t.Fatal("Authorization header not found in serialized request")
	}
	cut := pos + len("Autho")
	got := captureWrappedChunks(t, splitAt(raw, cut))
	_, names := headerNamesFromWire(t, got)
	assertOrder(t, names, claudeOAuthWireOrder)
	if !bytes.HasSuffix(got, []byte(body)) {
		t.Fatal("body not forwarded intact")
	}
}

// TestClaudeHeaderOrderConn_MultiChunkAccumulate feeds the whole header block
// across many small Write calls (< maxClaudeHeaderBlock), verifying multi-write
// accumulation still yields the claude-cli order.
func TestClaudeHeaderOrderConn_MultiChunkAccumulate(t *testing.T) {
	body := `{"model":"claude"}`
	raw := serializeClaudeRequest(t, newClaudeOAuthRequest(t, body))
	// 17-byte chunks force the ~600-800 byte head to accumulate over dozens of
	// Write calls, all well under maxClaudeHeaderBlock.
	var chunks [][]byte
	for off := 0; off < len(raw); off += 17 {
		end := off + 17
		if end > len(raw) {
			end = len(raw)
		}
		chunks = append(chunks, raw[off:end])
	}
	got := captureWrappedChunks(t, chunks)
	reqLine, names := headerNamesFromWire(t, got)
	if reqLine != "POST /v1/messages?beta=true HTTP/1.1" {
		t.Fatalf("request line = %q", reqLine)
	}
	assertOrder(t, names, claudeOAuthWireOrder)
	if !bytes.HasSuffix(got, []byte(body)) {
		t.Fatal("body not forwarded intact")
	}
}
