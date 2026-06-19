package helps

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	tls "github.com/refraction-networking/utls"
)

// expectedCodexRustlsJA3 / MD5 是真实 codex-rs 0.140.0 ClientHello 的 JA3（多次
// 抓样稳定）。抓样针对 127.0.0.1 且带 SNI，因此扩展列表里包含 server_name(0)。
// 注：rustls 会对 IP 字面量也发 SNI，而 uTLS 遵循 RFC 6066 对 IP 字面量不发 SNI；
// 为复现含扩展 0 的 JA3，测试用真实主机名 chatgpt.com（codex 生产目标）作 ServerName，
// 此时 uTLS 正常发 SNI。JA3 扩展段只记扩展类型(0)不记 SNI 值，故 hash 与目标一致。
const (
	expectedCodexRustlsJA3    = "771,255-49196-49195-49188-49187-49162-49161-49160-49200-49199-49192-49191-49172-49171-49170-157-156-61-60-53-47-10,0-10-11-13-5-18-23,23-24-25,0"
	expectedCodexRustlsJA3MD5 = "e4d448cdfe06dc1243c1eb026c74ac9a"
)

// TestCodexRustlsClientHelloMarshalsToTargetJA3 是本 wave 最强验证：把
// codex_rustls_native_v1 的 ClientHelloSpec 经 uTLS 实际 marshal 成线缆字节，再
// 按 JA3 规范从字节解析、计算 JA3，断言等于真实 codex-rs 的目标 hash。若 uTLS
// 偷偷注入 GREASE / ALPN / key_share / supported_versions(TLS1.3) / padding，
// 这些都会出现在线缆字节里，导致 JA3 不等、测试挂掉。
func TestCodexRustlsClientHelloMarshalsToTargetJA3(t *testing.T) {
	spec, err := newCodexRustlsClientHelloSpec()
	if err != nil {
		t.Fatalf("newCodexRustlsClientHelloSpec: %v", err)
	}

	// 通过 ApplyPreset + MarshalClientHello 得到真实线缆字节。ServerName 用真实
	// 主机名 chatgpt.com，使 uTLS 发出 SNI(0)，与目标 JA3 中的扩展 0 对齐。
	uConn := tls.UClient(nil, &tls.Config{ServerName: "chatgpt.com"}, tls.HelloCustom)
	if errApply := uConn.ApplyPreset(spec); errApply != nil {
		t.Fatalf("ApplyPreset: %v", errApply)
	}
	if errMarshal := uConn.MarshalClientHello(); errMarshal != nil {
		t.Fatalf("MarshalClientHello: %v", errMarshal)
	}
	raw := uConn.HandshakeState.Hello.Raw
	if len(raw) == 0 {
		t.Fatalf("marshalled ClientHello is empty")
	}

	ja3 := ja3FromMarshalledClientHello(t, raw)
	if ja3 != expectedCodexRustlsJA3 {
		t.Fatalf("codex JA3 string mismatch:\n got: %s\nwant: %s", ja3, expectedCodexRustlsJA3)
	}
	sum := md5.Sum([]byte(ja3))
	if got := hex.EncodeToString(sum[:]); got != expectedCodexRustlsJA3MD5 {
		t.Fatalf("codex JA3 md5 = %s, want %s", got, expectedCodexRustlsJA3MD5)
	}

	// 显式护栏：marshalled 字节里不允许出现这些会破坏 codex-rs 指纹的扩展。
	exts := parseClientHelloExtensions(t, raw)
	for _, banned := range []uint16{
		16,    // ALPN
		51,    // key_share
		43,    // supported_versions（会带回 TLS1.3）
		45,    // psk_key_exchange_modes
		35,    // session_ticket
		21,    // padding
		65281, // renegotiation_info（codex-rs 用 SCSV cipher，不用此扩展）
	} {
		if _, ok := exts[banned]; ok {
			t.Fatalf("codex ClientHello unexpectedly contains extension %d", banned)
		}
	}
	// client_version 必须是 TLS1.2(0x0303)。
	if ver := binary.BigEndian.Uint16(raw[4:6]); ver != 0x0303 {
		t.Fatalf("codex client_version = 0x%04x, want 0x0303 (TLS1.2)", ver)
	}
}

// TestCodexRustlsProfileResolvesToCustomDistinctSpec 验证 codex profile 解析为
// HelloCustom，但其 customSpecID 让 round tripper 构建 codex spec，而非 claude spec。
func TestCodexRustlsProfileResolvesToCustomDistinctSpec(t *testing.T) {
	got, ok := resolveClaudeClientHelloID(codexRustlsClientHelloProfileID)
	if !ok || got.Str() != tls.HelloCustom.Str() {
		t.Fatalf("codex profile resolves to %s, want HelloCustom", got.Str())
	}

	// codexCustomSpecID 只认 codex profile；claude / Chrome 都返回 ""。
	if codexCustomSpecID(codexRustlsClientHelloProfileID) != codexRustlsClientHelloProfileID {
		t.Fatalf("codexCustomSpecID(codex) did not select codex spec")
	}
	for _, other := range []string{
		claudeCLIClientHelloProfileID,
		"claude_cli_clienthello",
		"claude_node_openssl_v1",
		"claude_utls_chrome_133",
		"",
	} {
		if codexCustomSpecID(other) != "" {
			t.Fatalf("codexCustomSpecID(%q) leaked codex spec into a non-codex profile", other)
		}
	}

	// codex round tripper 实际构建 codex spec，且其 JA3 == 目标。
	rt := newUtlsRoundTripper("", got)
	rt.customSpecID = codexCustomSpecID(codexRustlsClientHelloProfileID)
	spec, err := rt.clientHelloSpec(got)
	if err != nil {
		t.Fatalf("codex round tripper clientHelloSpec: %v", err)
	}
	assertSpecJA3(t, spec, "chatgpt.com", expectedCodexRustlsJA3, expectedCodexRustlsJA3MD5)
}

// TestCodexDefaultClientUsesCodexRustlsNotChromeOrClaude 守护本 wave 的核心修复：
// codex-facing NewUtlsHTTPClient 默认不再错套 Chrome133，也不能是 claude-cli spec，
// 而是 codex-rs(rustls) spec（JA3 == 目标）。
func TestCodexDefaultClientUsesCodexRustlsNotChromeOrClaude(t *testing.T) {
	if utlsHTTPClientDefaultProfileID != codexRustlsClientHelloProfileID {
		t.Fatalf("codex-facing default = %q, want %q", utlsHTTPClientDefaultProfileID, codexRustlsClientHelloProfileID)
	}
	if utlsHTTPClientDefaultProfileID == claudeCLIClientHelloProfileID {
		t.Fatalf("codex-facing default must not be the claude-cli profile")
	}

	// 显式 profile 路径（4 个 codex call sites 用的就是这个）。
	codexClient := NewUtlsHTTPClientForProfile(context.Background(), nil, nil, 0, CodexRustlsClientHelloProfileID)
	codexRT := utlsRTFromClient(t, codexClient)
	if codexRT.clientHello.Str() != tls.HelloCustom.Str() {
		t.Fatalf("codex client ClientHello = %s, want HelloCustom", codexRT.clientHello.Str())
	}
	if codexRT.customSpecID != codexRustlsClientHelloProfileID {
		t.Fatalf("codex client customSpecID = %q, want codex", codexRT.customSpecID)
	}
	spec, err := codexRT.clientHelloSpec(codexRT.clientHello)
	if err != nil {
		t.Fatalf("codex client spec: %v", err)
	}
	assertSpecJA3(t, spec, "chatgpt.com", expectedCodexRustlsJA3, expectedCodexRustlsJA3MD5)

	// 默认无 profile 路径（NewUtlsHTTPClient）也应解析到 codex spec。
	defaultClient := NewUtlsHTTPClient(context.Background(), nil, nil, 0)
	defaultRT := utlsRTFromClient(t, defaultClient)
	if defaultRT.customSpecID != codexRustlsClientHelloProfileID {
		t.Fatalf("default codex client customSpecID = %q, want codex", defaultRT.customSpecID)
	}
}

// TestClaudeProfileUnaffectedByCodexChange 守护隔离：claude 路径仍解析到
// HelloChrome_133（claude_utls_chrome_133）以及 claude-cli HelloCustom spec，
// 不被 codex 改动污染。
func TestClaudeProfileUnaffectedByCodexChange(t *testing.T) {
	// claude_utls_chrome_133 仍 -> Chrome133。
	if got, ok := resolveClaudeClientHelloID("claude_utls_chrome_133"); !ok || got.Str() != tls.HelloChrome_133.Str() {
		t.Fatalf("claude_utls_chrome_133 = (%s, %v), want HelloChrome_133", got.Str(), ok)
	}

	// claude HelloCustom profile 路径构建的是 claude-cli spec（customSpecID 为空），
	// 其结构 JA3 仍是 claude 目标，不是 codex 目标。
	claudeRT := NewUtlsRoundTripperForProfile("", claudeCLIClientHelloProfileID)
	fr, ok := claudeRT.(*fallbackRoundTripper)
	if !ok {
		t.Fatalf("claude round tripper = %T, want *fallbackRoundTripper", claudeRT)
	}
	inner, ok := fr.utls.(*utlsRoundTripper)
	if !ok {
		t.Fatalf("claude protected transport = %T, want *utlsRoundTripper", fr.utls)
	}
	if inner.customSpecID != "" {
		t.Fatalf("claude round tripper customSpecID = %q, want empty (claude spec)", inner.customSpecID)
	}
	spec, err := inner.clientHelloSpec(inner.clientHello)
	if err != nil {
		t.Fatalf("claude spec: %v", err)
	}
	// 结构 JA3（排除 SNI）应等于 claude 目标，而绝不能等于 codex 目标。
	claudeStructural := buildStructuralJA3(t, spec)
	if claudeStructural != expectedClaudeCLIJA3 {
		t.Fatalf("claude structural JA3 changed:\n got: %s\nwant: %s", claudeStructural, expectedClaudeCLIJA3)
	}
	if claudeStructural == expectedCodexRustlsJA3 {
		t.Fatalf("claude spec collided with codex JA3 — isolation broken")
	}
}

// utlsRTFromClient 从 *http.Client 取出内部 *utlsRoundTripper（经 fallbackRoundTripper）。
func utlsRTFromClient(t *testing.T, client *http.Client) *utlsRoundTripper {
	t.Helper()
	fr, ok := client.Transport.(*fallbackRoundTripper)
	if !ok {
		t.Fatalf("client transport = %T, want *fallbackRoundTripper", client.Transport)
	}
	rt, ok := fr.utls.(*utlsRoundTripper)
	if !ok {
		t.Fatalf("protected transport = %T, want *utlsRoundTripper", fr.utls)
	}
	return rt
}

// assertSpecJA3 通过实际 marshal -> 解析 -> 计算 JA3，断言 spec 的 JA3 == 目标。
func assertSpecJA3(t *testing.T, spec *tls.ClientHelloSpec, serverName, wantJA3, wantMD5 string) {
	t.Helper()
	uConn := tls.UClient(nil, &tls.Config{ServerName: serverName}, tls.HelloCustom)
	if err := uConn.ApplyPreset(spec); err != nil {
		t.Fatalf("ApplyPreset: %v", err)
	}
	if err := uConn.MarshalClientHello(); err != nil {
		t.Fatalf("MarshalClientHello: %v", err)
	}
	ja3 := ja3FromMarshalledClientHello(t, uConn.HandshakeState.Hello.Raw)
	if ja3 != wantJA3 {
		t.Fatalf("JA3 string mismatch:\n got: %s\nwant: %s", ja3, wantJA3)
	}
	sum := md5.Sum([]byte(ja3))
	if got := hex.EncodeToString(sum[:]); got != wantMD5 {
		t.Fatalf("JA3 md5 = %s, want %s", got, wantMD5)
	}
}

// ja3FromMarshalledClientHello 从真实 marshalled ClientHello（handshake message
// 字节：[type(1)][len(3)][body...]）按 JA3 规范计算 JA3 字符串。算法与抓样所用
// /tmp/codex_ja3_listener.py 一致：版本,密码套件,扩展,曲线,点格式（去 GREASE）。
func ja3FromMarshalledClientHello(t *testing.T, raw []byte) string {
	t.Helper()
	p := newClientHelloReader(t, raw)

	clientVersion := p.u16() // client_version
	p.skip(32)               // random
	sidLen := int(p.u8())    // session_id
	p.skip(sidLen)

	csLen := int(p.u16())
	ciphers := make([]string, 0, csLen/2)
	for i := 0; i < csLen; i += 2 {
		c := p.u16()
		if isGREASEClientHello(c) {
			continue
		}
		ciphers = append(ciphers, strconv.Itoa(int(c)))
	}

	compLen := int(p.u8())
	p.skip(compLen)

	exts := make([]string, 0, 8)
	var curves, points []string
	if p.remaining() >= 2 {
		extTotal := int(p.u16())
		end := p.off + extTotal
		for p.off < end {
			et := p.u16()
			el := int(p.u16())
			ev := p.bytes(el)
			if isGREASEClientHello(et) {
				continue
			}
			exts = append(exts, strconv.Itoa(int(et)))
			switch et {
			case 0x000a: // supported_groups
				n := int(binary.BigEndian.Uint16(ev[0:2]))
				for i := 0; i < n; i += 2 {
					g := binary.BigEndian.Uint16(ev[2+i : 4+i])
					if isGREASEClientHello(g) {
						continue
					}
					curves = append(curves, strconv.Itoa(int(g)))
				}
			case 0x000b: // ec_point_formats
				n := int(ev[0])
				for i := 0; i < n; i++ {
					points = append(points, strconv.Itoa(int(ev[1+i])))
				}
			}
		}
	}

	return fmt.Sprintf("%d,%s,%s,%s,%s",
		clientVersion,
		strings.Join(ciphers, "-"),
		strings.Join(exts, "-"),
		strings.Join(curves, "-"),
		strings.Join(points, "-"),
	)
}

// parseClientHelloExtensions 返回 marshalled ClientHello 中出现的扩展集合。
func parseClientHelloExtensions(t *testing.T, raw []byte) map[uint16]struct{} {
	t.Helper()
	p := newClientHelloReader(t, raw)
	p.skip(2)            // client_version
	p.skip(32)           // random
	p.skip(int(p.u8()))  // session_id
	p.skip(int(p.u16())) // cipher_suites
	p.skip(int(p.u8()))  // compression_methods
	out := map[uint16]struct{}{}
	if p.remaining() < 2 {
		return out
	}
	extTotal := int(p.u16())
	end := p.off + extTotal
	for p.off < end {
		et := p.u16()
		el := int(p.u16())
		p.skip(el)
		out[et] = struct{}{}
	}
	return out
}

// clientHelloReader 是一个最小的大端字节游标，定位到 handshake message body。
type clientHelloReader struct {
	t    *testing.T
	data []byte
	off  int
}

func newClientHelloReader(t *testing.T, raw []byte) *clientHelloReader {
	t.Helper()
	if len(raw) < 4 {
		t.Fatalf("ClientHello too short: %d bytes", len(raw))
	}
	// raw[0] = handshake type(1=ClientHello), raw[1:4] = body length；body 从 4 起。
	if raw[0] != 0x01 {
		t.Fatalf("handshake type = %d, want 1 (ClientHello)", raw[0])
	}
	return &clientHelloReader{t: t, data: raw, off: 4}
}

func (p *clientHelloReader) u8() uint8 {
	p.t.Helper()
	if p.off+1 > len(p.data) {
		p.t.Fatalf("read u8 past end (off=%d)", p.off)
	}
	v := p.data[p.off]
	p.off++
	return v
}

func (p *clientHelloReader) u16() uint16 {
	p.t.Helper()
	if p.off+2 > len(p.data) {
		p.t.Fatalf("read u16 past end (off=%d)", p.off)
	}
	v := binary.BigEndian.Uint16(p.data[p.off : p.off+2])
	p.off += 2
	return v
}

func (p *clientHelloReader) skip(n int) {
	p.t.Helper()
	if p.off+n > len(p.data) {
		p.t.Fatalf("skip past end (off=%d n=%d)", p.off, n)
	}
	p.off += n
}

func (p *clientHelloReader) bytes(n int) []byte {
	p.t.Helper()
	if p.off+n > len(p.data) {
		p.t.Fatalf("read bytes past end (off=%d n=%d)", p.off, n)
	}
	b := p.data[p.off : p.off+n]
	p.off += n
	return b
}

func (p *clientHelloReader) remaining() int { return len(p.data) - p.off }
