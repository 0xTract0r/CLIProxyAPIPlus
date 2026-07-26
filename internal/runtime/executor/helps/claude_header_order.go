package helps

import (
	"bytes"
	"net"
	"strconv"
	"strings"

	utls "github.com/refraction-networking/utls"
)

// Route A: replay the real claude-cli (undici/Stainless) outbound HTTP/1.1
// request-header wire order AND original header-name casing, so the JA4H "_hd"
// (header-order) segment of claude egress matches a genuine claude-cli client
// instead of Go net/http's canonical Title-Case + alphabetical writeSubset order.
//
// Why a net.Conn wrapper instead of replacing the transport:
//   - Go's *http.Transport writes request headers via Request.write ->
//     Header.writeSubset, which sorts keys alphabetically and emits the
//     canonical (Title-Case) key stored at Set time. There is no hook to change
//     that ordering/casing at the http.Header level.
//   - Wrapping the uTLS net.Conn returned by dialTLSContext lets us rewrite only
//     the outgoing HTTP/1.1 request header block on the wire, while reusing 100%
//     of stdlib's connection pooling, keep-alive, chunked/response handling and
//     request cancellation. Reads are never touched, so streaming (SSE) responses
//     are unaffected. Gate-off = the conn is not wrapped at all = zero behavior
//     change.
//
// Scope: this is wired ONLY onto the claude serving/quota outbound transport
// (claude_cli_clienthello_v1 uTLS path) and only when the config flag is on.
// codex and gemini never construct a wrapped conn. The OAuth token
// exchange/refresh path uses a different endpoint with a different header set
// (not the /v1/messages SDK header profile) and is intentionally left untouched.
//
// Target order + casing captured zero-account from real claude-cli 2.1.220, both
// x-api-key and OAuth/Bearer modes, in
// docs/fingerprint/cpa-reqs/phase3-evidence/header-order-probe/
// (COMPARISON.txt + COMPARISON-oauth.txt). The table is the superset of both
// modes: Authorization sits between Accept and Content-Type (OAuth), x-api-key
// sits between anthropic-version and x-app (api-key); the two are mutually
// exclusive and each request emits whichever subset is actually present.

// claudeHeaderWireOrder is the canonical claude-cli outbound header order, keyed
// by lower-cased header name. The LAST four entries (connection, host,
// accept-encoding, content-length) are the terminal transport/undici headers
// that always come last; unknown headers are emitted just before them. Keep the
// terminal group contiguous at the tail: claudeTerminalHeaderCount depends on it.
var claudeHeaderWireOrder = []string{
	"accept",
	"authorization",
	"content-type",
	"user-agent",
	"x-claude-code-session-id",
	"x-stainless-arch",
	"x-stainless-lang",
	"x-stainless-os",
	"x-stainless-package-version",
	"x-stainless-retry-count",
	"x-stainless-runtime",
	"x-stainless-runtime-version",
	"x-stainless-timeout",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-api-key",
	"x-app",
	// terminal group (must stay last, contiguous):
	"connection",
	"host",
	"accept-encoding",
	"content-length",
}

// claudeTerminalHeaderCount is how many trailing entries of claudeHeaderWireOrder
// form the terminal group (Connection/Host/Accept-Encoding/Content-Length).
const claudeTerminalHeaderCount = 4

// claudeHeaderWireCase maps a lower-cased header name to the exact wire casing
// real claude-cli emits. Names not present here are unknown headers and keep the
// casing they were given upstream (they are also positioned as "unknown").
var claudeHeaderWireCase = map[string]string{
	"accept":                                    "Accept",
	"authorization":                             "Authorization",
	"content-type":                              "Content-Type",
	"user-agent":                                "User-Agent",
	"x-claude-code-session-id":                  "X-Claude-Code-Session-Id",
	"x-stainless-arch":                          "X-Stainless-Arch",
	"x-stainless-lang":                          "X-Stainless-Lang",
	"x-stainless-os":                            "X-Stainless-OS",
	"x-stainless-package-version":               "X-Stainless-Package-Version",
	"x-stainless-retry-count":                   "X-Stainless-Retry-Count",
	"x-stainless-runtime":                       "X-Stainless-Runtime",
	"x-stainless-runtime-version":               "X-Stainless-Runtime-Version",
	"x-stainless-timeout":                       "X-Stainless-Timeout",
	"anthropic-beta":                            "anthropic-beta",
	"anthropic-dangerous-direct-browser-access": "anthropic-dangerous-direct-browser-access",
	"anthropic-version":                         "anthropic-version",
	"x-api-key":                                 "x-api-key",
	"x-app":                                     "x-app",
	"connection":                                "Connection",
	"host":                                      "Host",
	"accept-encoding":                           "Accept-Encoding",
	"content-length":                            "Content-Length",
}

// claudeHeaderOrderIndex maps a lower-cased header name to its position in
// claudeHeaderWireOrder for O(1) lookup. Built once at init.
var claudeHeaderOrderIndex = func() map[string]int {
	idx := make(map[string]int, len(claudeHeaderWireOrder))
	for i, name := range claudeHeaderWireOrder {
		idx[name] = i
	}
	return idx
}()

const (
	crlf         = "\r\n"
	headerBlkEnd = "\r\n\r\n"
	// maxClaudeHeaderBlock bounds how many bytes we buffer looking for the end of
	// a request header block before giving up and passing bytes through untouched.
	// Real claude request heads are ~1-2 KB; a generous ceiling avoids unbounded
	// buffering on a malformed/unexpected stream while never truncating a real head.
	maxClaudeHeaderBlock = 128 * 1024
)

// reorderClaudeRequestHead rewrites one HTTP/1.1 request header block (from the
// request line through the terminating CRLFCRLF) into the real claude-cli wire
// order + casing. block MUST include the trailing "\r\n\r\n". It returns the
// rewritten block, the parsed Content-Length (bodyLen; 0 when absent/GET/HEAD),
// and chunked=true when Transfer-Encoding: chunked is present (in which case the
// caller should stop reordering and pass the rest of the connection through,
// since the body length cannot be counted without chunk parsing).
//
// It never drops or duplicates a header: every parsed header is emitted exactly
// once. Header values are preserved byte-for-byte (only the name casing and the
// line order change). Headers not in claudeHeaderWireOrder ("unknown") are
// emitted, in their original relative order, immediately before the terminal
// Connection/Host/Accept-Encoding/Content-Length group.
func reorderClaudeRequestHead(block []byte) (out []byte, bodyLen int64, chunked bool) {
	sepIdx := bytes.Index(block, []byte(headerBlkEnd))
	if sepIdx < 0 {
		// Caller contract violated; return unchanged so we never corrupt the wire.
		return block, 0, false
	}
	head := block[:sepIdx] // request line + header lines, no trailing CRLFCRLF
	lines := strings.Split(string(head), crlf)
	if len(lines) == 0 {
		return block, 0, false
	}
	requestLine := lines[0]

	// Self-protection: only ever rewrite genuine HTTP/1.1 request heads. This
	// keeps the "this conn only speaks HTTP/1.1" safety local to the rewriter,
	// independent of the transport's ALPN/ForceAttemptHTTP2 config. If a future
	// change ever let this uTLS transport negotiate h2, the HTTP/2 connection
	// preface ("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n") also contains "\r\n\r\n"; a
	// non-HTTP/1.1 request line here means we must NOT reparse/reorder it as
	// headers. Returning the block unchanged forwards those bytes byte-for-byte.
	if !strings.HasSuffix(requestLine, " HTTP/1.1") {
		return block, 0, false
	}

	type hdr struct {
		lower string // lower-cased name; "" for a malformed line with no colon
		text  string // full rewritten line (name re-cased + original value bytes)
	}
	collected := make([]hdr, 0, len(lines))
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			// Malformed header line (no colon). Keep verbatim, treat as unknown.
			collected = append(collected, hdr{lower: "", text: line})
			continue
		}
		name := line[:colon]
		rest := line[colon:] // includes ":" and the original value bytes
		lower := strings.ToLower(strings.TrimSpace(name))

		switch lower {
		case "content-length":
			if n, err := strconv.ParseInt(strings.TrimSpace(line[colon+1:]), 10, 64); err == nil && n >= 0 {
				bodyLen = n
			}
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(line[colon+1:]), "chunked") {
				chunked = true
			}
		}

		wireName := name
		if cased, ok := claudeHeaderWireCase[lower]; ok {
			wireName = cased
		}
		collected = append(collected, hdr{lower: lower, text: wireName + rest})
	}

	var buf bytes.Buffer
	buf.Grow(len(block) + 64)
	buf.WriteString(requestLine)
	buf.WriteString(crlf)

	emitted := make([]bool, len(collected))
	firstTerminal := len(claudeHeaderWireOrder) - claudeTerminalHeaderCount

	// 1. Leading known headers, in table order (input order preserved for dups).
	for oi := 0; oi < firstTerminal; oi++ {
		name := claudeHeaderWireOrder[oi]
		for i := range collected {
			if !emitted[i] && collected[i].lower == name {
				buf.WriteString(collected[i].text)
				buf.WriteString(crlf)
				emitted[i] = true
			}
		}
	}
	// 2. Unknown headers (not in the table), in original relative order, placed
	//    before the terminal group.
	for i := range collected {
		if emitted[i] {
			continue
		}
		if _, known := claudeHeaderOrderIndex[collected[i].lower]; known {
			continue
		}
		buf.WriteString(collected[i].text)
		buf.WriteString(crlf)
		emitted[i] = true
	}
	// 3. Terminal known headers, in table order.
	for oi := firstTerminal; oi < len(claudeHeaderWireOrder); oi++ {
		name := claudeHeaderWireOrder[oi]
		for i := range collected {
			if !emitted[i] && collected[i].lower == name {
				buf.WriteString(collected[i].text)
				buf.WriteString(crlf)
				emitted[i] = true
			}
		}
	}

	// Each header line above already carries its trailing CRLF, so the header
	// block is terminated by a single empty-line CRLF (which combines with the
	// last header's CRLF to form the on-wire CRLFCRLF).
	buf.WriteString(crlf)
	return buf.Bytes(), bodyLen, chunked
}

// claude header-order conn write states.
const (
	hdrStateAccumulate = iota // buffering the current request header block
	hdrStateBody              // forwarding exactly bodyLeft body bytes (or all, if bodyLeft<0)
)

// claudeHeaderOrderConn wraps a net.Conn and rewrites the outgoing HTTP/1.1
// request header block(s) into the real claude-cli wire order + casing. Only
// Write is intercepted; every other net.Conn method (Read/Close/deadlines/addrs)
// is inherited from the embedded conn, so streaming reads and connection
// lifecycle are unchanged. It is stateful across writes to handle bufio flush
// splits and keep-alive connection reuse (one request at a time; Go does not
// pipeline HTTP/1.1).
type claudeHeaderOrderConn struct {
	net.Conn

	state    int
	buf      []byte // accumulated header-block bytes while in hdrStateAccumulate
	bodyLeft int64  // remaining body bytes in hdrStateBody; <0 means "pass through everything"
}

func newClaudeHeaderOrderConn(inner net.Conn) net.Conn {
	return &claudeHeaderOrderConn{Conn: inner, state: hdrStateAccumulate}
}

// ConnectionState forwards the underlying uTLS conn's TLS ConnectionState so
// wrapping does not hide that optional interface from any uTLS-aware consumer.
// Embedding net.Conn alone would only promote net.Conn methods and drop
// ConnectionState, creating a gate-on vs gate-off difference in the interfaces
// the conn satisfies. Forwarding keeps parity. (Note: the Go net/http transport
// asserts for the stdlib crypto/tls.ConnectionState type, which a refraction
// uTLS conn never satisfies, so resp.TLS stays nil in both modes regardless;
// this method restores parity for uTLS-typed consumers only.)
func (c *claudeHeaderOrderConn) ConnectionState() utls.ConnectionState {
	if cs, ok := c.Conn.(interface {
		ConnectionState() utls.ConnectionState
	}); ok {
		return cs.ConnectionState()
	}
	return utls.ConnectionState{}
}

// Write intercepts the request header block and re-emits it in claude-cli order.
// It always reports len(p) consumed on success (the classic transforming-writer
// contract); on an underlying write error it returns that error. Body bytes are
// forwarded untouched.
func (c *claudeHeaderOrderConn) Write(p []byte) (int, error) {
	if err := c.write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *claudeHeaderOrderConn) write(p []byte) error {
	for len(p) > 0 {
		if c.state == hdrStateBody {
			n := len(p)
			if c.bodyLeft >= 0 && int64(n) > c.bodyLeft {
				n = int(c.bodyLeft)
			}
			if n > 0 {
				if _, err := c.Conn.Write(p[:n]); err != nil {
					return err
				}
				if c.bodyLeft >= 0 {
					c.bodyLeft -= int64(n)
				}
				p = p[n:]
			}
			if c.bodyLeft == 0 {
				c.state = hdrStateAccumulate
				c.buf = c.buf[:0]
			}
			continue
		}

		// hdrStateAccumulate: append and look for the end of the header block.
		c.buf = append(c.buf, p...)
		p = nil
		idx := bytes.Index(c.buf, []byte(headerBlkEnd))
		if idx < 0 {
			if len(c.buf) > maxClaudeHeaderBlock {
				// Unexpectedly large / malformed head: flush untouched and stop
				// reordering for the remainder of this connection rather than
				// buffer unboundedly or corrupt the stream.
				if _, err := c.Conn.Write(c.buf); err != nil {
					return err
				}
				c.buf = nil
				c.state = hdrStateBody
				c.bodyLeft = -1
			}
			return nil
		}

		block := c.buf[:idx+len(headerBlkEnd)]
		rest := append([]byte(nil), c.buf[idx+len(headerBlkEnd):]...)
		out, bodyLen, chunked := reorderClaudeRequestHead(block)
		if _, err := c.Conn.Write(out); err != nil {
			return err
		}
		c.buf = c.buf[:0]

		if chunked {
			// Body length is not countable without chunk parsing; pass the rest
			// of the connection through untouched. Claude serving never sends
			// chunked request bodies (Content-Length is always set), so this is a
			// safety fallback, not a normal path.
			c.state = hdrStateBody
			c.bodyLeft = -1
		} else {
			c.state = hdrStateBody
			c.bodyLeft = bodyLen
			if bodyLen == 0 {
				c.state = hdrStateAccumulate
			}
		}
		p = rest
	}
	return nil
}
