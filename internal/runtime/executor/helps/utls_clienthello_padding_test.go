package helps

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"

	tls "github.com/refraction-networking/utls"
)

// T020 anti-correlation gap: the real claude-cli (Node/OpenSSL) appends an
// RFC7685 padding extension (21/0x15) only when the unpadded ClientHello length
// lands in [256,511] bytes (BoringSSL convention). The replicated spec was
// missing it, so CPA's real egress to api.anthropic.com (with SNI) produced
// JA3 dc782a9d... instead of the real client's d871d02c..., differing by
// exactly this one extension. The no-SNI ClientHello stays < 256 bytes, so it
// must NOT be padded and its structural JA3 must remain e97f5146...
//
// These full md5 values were derived by running the repo JA3 caliber
// (docs/fingerprint/cpa-reqs/phase3-evidence/ja3_from_pcap.py, GREASE stripped,
// fields = version,ciphers,exts,curves,formats) over the real captured
// claude-cli ClientHello bytes (host /tmp/ja3-refs/ch-*.bin):
//   - with-SNI (docker claude 2.1.177 / local 2.1.173): d871d02cecbde59abbf8f4806134addf
//   - no-SNI   (local claude 2.1.173):                   e97f5146a7009cc2918b50e903b6ff8d
const (
	expectedClaudeCLIWithSNIJA3MD5 = "d871d02cecbde59abbf8f4806134addf"
	expectedClaudeCLINoSNIJA3MD5   = "e97f5146a7009cc2918b50e903b6ff8d"
)

// marshalledClientHelloJA3MD5 builds the actual ClientHello via the real
// newClaudeCLIClientHelloSpec, applies it through a uTLS UClient with the given
// SNI, marshals the handshake bytes, and computes JA3 over the wire bytes
// (so conditional padding is reflected). serverName "" means a no-SNI
// (connect-by-IP) ClientHello, achieved by leaving Config.ServerName empty.
func marshalledClientHelloJA3MD5(t *testing.T, serverName string) (string, []byte) {
	t.Helper()

	spec, err := newClaudeCLIClientHelloSpec()
	if err != nil {
		t.Fatalf("newClaudeCLIClientHelloSpec: %v", err)
	}

	cfg := &tls.Config{}
	if serverName != "" {
		cfg.ServerName = serverName
	} else {
		// Skip verify is irrelevant (no real handshake happens); InsecureSkipVerify
		// avoids any SNI being inferred. An empty ServerName yields a no-SNI
		// ClientHello, mirroring a connect-by-IP egress.
		cfg.InsecureSkipVerify = true
	}

	uConn := tls.UClient(nil, cfg, tls.HelloCustom)
	if errApply := uConn.ApplyPreset(spec); errApply != nil {
		t.Fatalf("ApplyPreset(serverName=%q): %v", serverName, errApply)
	}
	if errBuild := uConn.BuildHandshakeState(); errBuild != nil {
		t.Fatalf("BuildHandshakeState(serverName=%q): %v", serverName, errBuild)
	}

	raw := uConn.HandshakeState.Hello.Raw
	if len(raw) == 0 {
		t.Fatalf("marshalled ClientHello is empty (serverName=%q)", serverName)
	}
	return ja3MD5FromHandshakeMsg(t, raw), raw
}

// ja3MD5FromHandshakeMsg parses a marshalled handshake-layer ClientHello
// message (starting with HandshakeType client_hello = 0x01, then 3-byte length,
// then the ClientHello body) and computes the JA3 md5. Algorithm matches the
// repo caliber in ja3_from_pcap.py: GREASE stripped from ciphers/extensions/
// groups; JA3 string = version,ciphers,extensions,curves,ec_point_formats;
// md5 of that string.
func ja3MD5FromHandshakeMsg(t *testing.T, hs []byte) string {
	t.Helper()

	if len(hs) < 4 || hs[0] != 0x01 {
		t.Fatalf("not a client_hello handshake message: len=%d first=0x%02x", len(hs), firstByte(hs))
	}
	// Skip handshake type (1) + length (3).
	p := 4
	clientVer := int(uint16(hs[p])<<8 | uint16(hs[p+1]))
	p += 2
	// random (32)
	p += 32
	// session id
	sidLen := int(hs[p])
	p++
	p += sidLen
	// cipher suites
	csLen := int(uint16(hs[p])<<8 | uint16(hs[p+1]))
	p += 2
	var ciphers []int
	for i := 0; i < csLen; i += 2 {
		ciphers = append(ciphers, int(uint16(hs[p+i])<<8|uint16(hs[p+i+1])))
	}
	p += csLen
	// compression methods
	compLen := int(hs[p])
	p++
	p += compLen
	// extensions
	extTotal := int(uint16(hs[p])<<8 | uint16(hs[p+1]))
	p += 2
	extEnd := p + extTotal

	var exts, groups, ecFmts []int
	for p+4 <= extEnd && p+4 <= len(hs) {
		et := int(uint16(hs[p])<<8 | uint16(hs[p+1]))
		el := int(uint16(hs[p+2])<<8 | uint16(hs[p+3]))
		p += 4
		body := hs[p : p+el]
		p += el
		exts = append(exts, et)
		switch et {
		case 0x000a: // supported_groups
			gl := int(uint16(body[0])<<8 | uint16(body[1]))
			for i := 0; i < gl; i += 2 {
				groups = append(groups, int(uint16(body[2+i])<<8|uint16(body[3+i])))
			}
		case 0x000b: // ec_point_formats
			fl := int(body[0])
			for i := 0; i < fl; i++ {
				ecFmts = append(ecFmts, int(body[1+i]))
			}
		}
	}

	ja3 := fmt.Sprintf("%d,%s,%s,%s,%s",
		clientVer,
		joinInts(stripGREASE(ciphers)),
		joinInts(stripGREASE(exts)),
		joinInts(stripGREASE(groups)),
		joinInts(ecFmts),
	)
	sum := md5.Sum([]byte(ja3))
	return hex.EncodeToString(sum[:])
}

func firstByte(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}

func isGREASEValue(v int) bool {
	u := uint16(v)
	return (u&0x0f0f) == 0x0a0a && (u>>8) == (u&0xff)
}

func stripGREASE(in []int) []int {
	out := make([]int, 0, len(in))
	for _, v := range in {
		if isGREASEValue(v) {
			continue
		}
		out = append(out, v)
	}
	return out
}

func joinInts(in []int) string {
	parts := make([]string, 0, len(in))
	for _, v := range in {
		parts = append(parts, strconv.Itoa(v))
	}
	return strings.Join(parts, "-")
}

// TestClaudeCLIClientHelloWithSNIJA3HasConditionalPadding locks the T020 fix:
// the real-host (SNI) ClientHello must include padding(21) and match the real
// claude-cli with-SNI JA3 d871d02c..., while the no-SNI ClientHello must NOT be
// padded and must keep the original aligned JA3 e97f5146...
func TestClaudeCLIClientHelloWithSNIJA3HasConditionalPadding(t *testing.T) {
	withSNI, rawWith := marshalledClientHelloJA3MD5(t, "api.anthropic.com")
	if withSNI != expectedClaudeCLIWithSNIJA3MD5 {
		t.Fatalf("with-SNI JA3 md5 = %s, want %s (real claude-cli with-SNI); padding(21) missing or wrong",
			withSNI, expectedClaudeCLIWithSNIJA3MD5)
	}
	if !extensionPresent(t, rawWith, 0x0015) {
		t.Fatalf("with-SNI ClientHello is missing the padding(21/0x15) extension")
	}

	noSNI, rawNo := marshalledClientHelloJA3MD5(t, "")
	if noSNI != expectedClaudeCLINoSNIJA3MD5 {
		t.Fatalf("no-SNI JA3 md5 = %s, want %s (must stay unchanged); padding must NOT be applied below 256 bytes",
			noSNI, expectedClaudeCLINoSNIJA3MD5)
	}
	if extensionPresent(t, rawNo, 0x0015) {
		t.Fatalf("no-SNI ClientHello unexpectedly contains padding(21/0x15); BoringPaddingStyle must stay conditional")
	}

	t.Logf("with-SNI JA3=%s (len(CH msg body)=%d incl padding)", withSNI, len(rawWith))
	t.Logf("no-SNI  JA3=%s (len(CH msg body)=%d, no padding)", noSNI, len(rawNo))
}

// extensionPresent reports whether the marshalled handshake-layer ClientHello
// advertises the given extension type.
func extensionPresent(t *testing.T, hs []byte, want int) bool {
	t.Helper()
	if len(hs) < 4 || hs[0] != 0x01 {
		t.Fatalf("not a client_hello handshake message")
	}
	p := 4
	p += 2      // version
	p += 32     // random
	p += int(hs[p]) + 1 // session id (len byte + body)
	csLen := int(uint16(hs[p])<<8 | uint16(hs[p+1]))
	p += 2 + csLen
	p += int(hs[p]) + 1 // compression (len byte + body)
	p += 2              // extensions total length
	for p+4 <= len(hs) {
		et := int(uint16(hs[p])<<8 | uint16(hs[p+1]))
		el := int(uint16(hs[p+2])<<8 | uint16(hs[p+3]))
		p += 4 + el
		if et == want {
			return true
		}
	}
	return false
}
