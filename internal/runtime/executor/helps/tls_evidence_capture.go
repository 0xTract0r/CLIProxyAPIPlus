package helps

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

type SyntheticProviderSNIEvidence struct {
	EvidenceType      string                   `json:"evidence_type"`
	Provider          string                   `json:"provider"`
	ProviderHost      string                   `json:"provider_host"`
	ProviderSNI       string                   `json:"provider_sni"`
	RequestURL        string                   `json:"request_url"`
	RequestSummary    map[string]any           `json:"request_summary"`
	RuntimeProfile    *RuntimeTransportProfile `json:"runtime_profile,omitempty"`
	TLS               ClientHelloEvidence      `json:"tls"`
	JA3               FingerprintEvidence      `json:"ja3"`
	JA4               FingerprintEvidence      `json:"ja4"`
	ALPN              ALPNEvidence             `json:"alpn"`
	HTTP2             HTTP2SettingsEvidence    `json:"http2"`
	Limitations       []string                 `json:"limitations"`
	GeneratedAt       string                   `json:"generated_at"`
	ProbeTransport    string                   `json:"probe_transport"`
	ProviderHostClaim string                   `json:"provider_host_claim"`
	RequestError      string                   `json:"request_error,omitempty"`
}

type ClientHelloEvidence struct {
	ClientHelloCaptured bool     `json:"client_hello_captured"`
	LegacyVersion       string   `json:"legacy_version"`
	HighestTLSVersion   string   `json:"highest_tls_version"`
	ServerName          string   `json:"server_name"`
	CipherSuites        []string `json:"cipher_suites"`
	Extensions          []string `json:"extensions"`
	SupportedGroups     []string `json:"supported_groups,omitempty"`
	PointFormats        []string `json:"ec_point_formats,omitempty"`
	SignatureSchemes    []string `json:"signature_schemes,omitempty"`
	ALPNProtocols       []string `json:"alpn_protocols,omitempty"`
	RawRecordSHA256     string   `json:"raw_record_sha256"`
	ParseError          string   `json:"parse_error,omitempty"`
}

type FingerprintEvidence struct {
	Value     string `json:"value,omitempty"`
	String    string `json:"string,omitempty"`
	Hash      string `json:"hash,omitempty"`
	Algorithm string `json:"algorithm"`
}

type ALPNEvidence struct {
	Offered    []string `json:"offered"`
	Negotiated string   `json:"negotiated"`
}

type HTTP2SettingsEvidence struct {
	Available bool              `json:"available"`
	Reason    string            `json:"reason,omitempty"`
	Settings  map[string]uint32 `json:"settings,omitempty"`
	Raw       []HTTP2Setting    `json:"raw,omitempty"`
}

type HTTP2Setting struct {
	ID    uint16 `json:"id"`
	Name  string `json:"name"`
	Value uint32 `json:"value"`
}

func CaptureSyntheticProviderSNIEvidence(ctx context.Context, auth *cliproxyauth.Auth, providerHost string) (*SyntheticProviderSNIEvidence, error) {
	providerHost = strings.ToLower(strings.TrimSpace(providerHost))
	if providerHost == "" {
		return nil, fmt.Errorf("provider host is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		return nil, fmt.Errorf("start local TLS capture listener: %w", errListen)
	}
	defer listener.Close()

	serverDone := make(chan localCaptureResult, 1)
	go serveLocalTLSEvidenceCapture(listener, providerHost, serverDone)

	rt, profile, limitation, errBuild := BuildTLSEvidenceProbeRoundTripperForLocalAddress(auth, listener.Addr().String())
	if errBuild != nil {
		return nil, errBuild
	}

	requestURL := "https://" + providerHost + "/tls-evidence-probe"
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if errReq != nil {
		return nil, errReq
	}
	req.Header.Set("Accept", "application/json")

	requestSummary := map[string]any{
		"method": "GET",
		"url":    requestURL,
	}

	client := &http.Client{Transport: rt}
	resp, errDo := client.Do(req)
	requestError := ""
	if errDo != nil {
		requestError = errDo.Error()
	} else if resp != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		if errClose := resp.Body.Close(); errClose != nil && requestError == "" {
			requestError = errClose.Error()
		}
		requestSummary["status_code"] = resp.StatusCode
		requestSummary["status"] = resp.Status
	}

	var capture localCaptureResult
	select {
	case capture = <-serverDone:
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for local TLS capture: %w", ctx.Err())
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("wait for local TLS capture: timeout")
	}
	if capture.err != nil {
		return nil, capture.err
	}

	limitations := []string{
		"evidence_type=synthetic-provider-sni: local TLS listener captured ClientHello with provider SNI; this is not provider-side observed traffic",
		"synthetic auth metadata was used; no tokens were read",
		"local capture overrides DNS/TCP dial target to 127.0.0.1 and uses an ephemeral self-signed certificate",
	}
	if limitation != "" {
		limitations = append(limitations, limitation)
	}

	provider := ""
	if auth != nil {
		provider = auth.Provider
	}
	return &SyntheticProviderSNIEvidence{
		EvidenceType:      "synthetic-provider-sni",
		Provider:          provider,
		ProviderHost:      providerHost,
		ProviderSNI:       capture.clientHello.ServerName,
		RequestURL:        requestURL,
		RequestSummary:    requestSummary,
		RuntimeProfile:    profile,
		TLS:               capture.clientHello,
		JA3:               buildJA3Evidence(capture.clientHello),
		JA4:               buildJA4Evidence(capture.clientHello),
		ALPN:              ALPNEvidence{Offered: capture.clientHello.ALPNProtocols, Negotiated: capture.negotiatedProtocol},
		HTTP2:             capture.http2,
		Limitations:       limitations,
		GeneratedAt:       time.Now().UTC().Format(time.RFC3339),
		ProbeTransport:    fmt.Sprintf("%T", rt),
		ProviderHostClaim: "synthetic-provider-sni: local capture used provider host as URL/SNI only; not provider-side observation",
		RequestError:      requestError,
	}, nil
}

func BuildTLSEvidenceProbeRoundTripperForLocalAddress(auth *cliproxyauth.Auth, localAddr string) (http.RoundTripper, *RuntimeTransportProfile, string, error) {
	if strings.TrimSpace(localAddr) == "" {
		return nil, nil, "", fmt.Errorf("local address is required")
	}
	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil || !profile.SupportsRuntime() {
		return nil, profile, "", fmt.Errorf("runtime transport profile is not configured or unsupported")
	}

	switch profile.Provider {
	case "claude":
		clientHelloProfile := profile.TLSProfileID
		if clientHelloProfile == "" {
			clientHelloProfile = profile.ProfileID
		}
		clientHello, ok := resolveClaudeClientHelloID(clientHelloProfile)
		if !ok {
			clientHello, _ = resolveClaudeClientHelloID("claude_utls_chrome_133")
		}
		limitation := "diagnostic local capture: reuses resolved Claude uTLS ClientHello with provider SNI and local TCP dial override"
		return newDiagnosticUtlsRoundTripper(localAddrDialer{addr: localAddr}, clientHello), profile, limitation, nil
	case "codex":
		rt := NewCodexTransportRoundTripperForProfile("", profile.ProfileID, profile.ALPN, profile.ForceHTTP11)
		transport, ok := rt.(*http.Transport)
		if !ok || transport == nil {
			return nil, profile, "", fmt.Errorf("codex diagnostic capture requires *http.Transport, got %T", rt)
		}
		cloned := transport.Clone()
		cloned.Proxy = nil
		cloned.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialer := &net.Dialer{}
			return dialer.DialContext(ctx, "tcp", localAddr)
		}
		if cloned.TLSClientConfig == nil {
			cloned.TLSClientConfig = &tls.Config{}
		} else {
			cloned.TLSClientConfig = cloned.TLSClientConfig.Clone()
		}
		cloned.TLSClientConfig.InsecureSkipVerify = true
		return cloned, profile, "diagnostic local capture: reuses Codex Go transport profile with local TCP dial override", nil
	default:
		return nil, profile, "", fmt.Errorf("unsupported provider %q", strings.TrimSpace(profile.Provider))
	}
}

type localAddrDialer struct {
	addr string
}

func (d localAddrDialer) Dial(network, addr string) (net.Conn, error) {
	return net.DialTimeout("tcp", d.addr, 5*time.Second)
}

var _ proxy.Dialer = localAddrDialer{}

type localCaptureResult struct {
	clientHello        ClientHelloEvidence
	negotiatedProtocol string
	http2              HTTP2SettingsEvidence
	err                error
}

func serveLocalTLSEvidenceCapture(listener net.Listener, providerHost string, done chan<- localCaptureResult) {
	rawConn, errAccept := listener.Accept()
	if errAccept != nil {
		done <- localCaptureResult{err: fmt.Errorf("accept local TLS capture connection: %w", errAccept)}
		return
	}
	defer rawConn.Close()

	reader := bufio.NewReader(rawConn)
	clientHello, errParse := peekClientHello(reader)
	if errParse != nil {
		clientHello.ParseError = errParse.Error()
	}

	cert, errCert := selfSignedCertificate(providerHost)
	if errCert != nil {
		done <- localCaptureResult{clientHello: clientHello, err: errCert}
		return
	}
	tlsConn := tls.Server(&bufferedConn{Conn: rawConn, reader: reader}, &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2", "http/1.1"},
	})
	if errHandshake := tlsConn.Handshake(); errHandshake != nil {
		done <- localCaptureResult{clientHello: clientHello, err: fmt.Errorf("local TLS handshake: %w", errHandshake)}
		return
	}

	state := tlsConn.ConnectionState()
	result := localCaptureResult{
		clientHello:        clientHello,
		negotiatedProtocol: state.NegotiatedProtocol,
	}
	switch state.NegotiatedProtocol {
	case "h2":
		result.http2 = serveHTTP2Capture(tlsConn)
	case "http/1.1", "":
		result.http2 = HTTP2SettingsEvidence{Available: false, Reason: "http2 was not negotiated"}
		serveHTTP1Once(tlsConn)
	default:
		result.http2 = HTTP2SettingsEvidence{Available: false, Reason: "unexpected ALPN protocol " + state.NegotiatedProtocol}
	}
	done <- result
}

func serveHTTP2Capture(conn net.Conn) HTTP2SettingsEvidence {
	capture := &http2SettingsCapture{}
	wrapped := &plaintextCaptureConn{Conn: conn, capture: capture}
	server := &http2.Server{IdleTimeout: 500 * time.Millisecond}
	served := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"evidence_type":"synthetic-provider-sni"}`))
		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = conn.Close()
		}()
	})
	go func() {
		server.ServeConn(wrapped, &http2.ServeConnOpts{Handler: handler})
		close(served)
	}()
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		_ = conn.Close()
		<-served
	}
	return capture.evidence()
}

func serveHTTP1Once(conn net.Conn) {
	reader := bufio.NewReader(conn)
	req, errReq := http.ReadRequest(reader)
	if errReq == nil && req != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 52\r\nConnection: close\r\n\r\n{\"ok\":true,\"evidence_type\":\"synthetic-provider-sni\"}"))
	}
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

type plaintextCaptureConn struct {
	net.Conn
	capture *http2SettingsCapture
}

func (c *plaintextCaptureConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.capture != nil {
		c.capture.feed(p[:n])
	}
	return n, err
}

type http2SettingsCapture struct {
	mu       sync.Mutex
	buffer   []byte
	settings []HTTP2Setting
	done     bool
}

func (c *http2SettingsCapture) feed(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return
	}
	c.buffer = append(c.buffer, data...)
	preface := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	if len(c.buffer) < len(preface)+9 {
		return
	}
	if !bytes.Equal(c.buffer[:len(preface)], preface) {
		c.done = true
		return
	}
	offset := len(preface)
	for len(c.buffer[offset:]) >= 9 {
		header := c.buffer[offset : offset+9]
		length := int(header[0])<<16 | int(header[1])<<8 | int(header[2])
		frameType := header[3]
		flags := header[4]
		if len(c.buffer[offset+9:]) < length {
			return
		}
		payload := c.buffer[offset+9 : offset+9+length]
		offset += 9 + length
		if frameType != 0x4 || flags&0x1 != 0 {
			continue
		}
		for i := 0; i+6 <= len(payload); i += 6 {
			id := uint16(payload[i])<<8 | uint16(payload[i+1])
			value := uint32(payload[i+2])<<24 | uint32(payload[i+3])<<16 | uint32(payload[i+4])<<8 | uint32(payload[i+5])
			c.settings = append(c.settings, HTTP2Setting{ID: id, Name: http2SettingName(id), Value: value})
		}
		c.done = true
		return
	}
}

func (c *http2SettingsCapture) evidence() HTTP2SettingsEvidence {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.settings) == 0 {
		return HTTP2SettingsEvidence{Available: false, Reason: "http2 was negotiated but client SETTINGS were not captured before connection close"}
	}
	settings := make(map[string]uint32, len(c.settings))
	for _, setting := range c.settings {
		settings[setting.Name] = setting.Value
	}
	return HTTP2SettingsEvidence{Available: true, Settings: settings, Raw: append([]HTTP2Setting(nil), c.settings...)}
}

func http2SettingName(id uint16) string {
	switch id {
	case 1:
		return "HEADER_TABLE_SIZE"
	case 2:
		return "ENABLE_PUSH"
	case 3:
		return "MAX_CONCURRENT_STREAMS"
	case 4:
		return "INITIAL_WINDOW_SIZE"
	case 5:
		return "MAX_FRAME_SIZE"
	case 6:
		return "MAX_HEADER_LIST_SIZE"
	case 8:
		return "ENABLE_CONNECT_PROTOCOL"
	default:
		return fmt.Sprintf("UNKNOWN_%d", id)
	}
}

func peekClientHello(reader *bufio.Reader) (ClientHelloEvidence, error) {
	header, errPeek := reader.Peek(5)
	if errPeek != nil {
		return ClientHelloEvidence{}, errPeek
	}
	if len(header) != 5 || header[0] != 22 {
		return ClientHelloEvidence{}, fmt.Errorf("first TLS record is not a handshake record")
	}
	recordLen := int(header[3])<<8 | int(header[4])
	record, errPeek := reader.Peek(5 + recordLen)
	if errPeek != nil {
		return ClientHelloEvidence{}, errPeek
	}
	return parseClientHello(record)
}

func parseClientHello(record []byte) (ClientHelloEvidence, error) {
	out := ClientHelloEvidence{
		ClientHelloCaptured: true,
		RawRecordSHA256:     sha256HexLocal(record),
	}
	if len(record) < 9 {
		return out, fmt.Errorf("short TLS record")
	}
	payload := record[5:]
	if payload[0] != 1 {
		return out, fmt.Errorf("first handshake message is not ClientHello")
	}
	handshakeLen := int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
	if len(payload) < 4+handshakeLen {
		return out, fmt.Errorf("short ClientHello payload")
	}
	body := payload[4 : 4+handshakeLen]
	cursor := 0
	if len(body) < 2+32+1 {
		return out, fmt.Errorf("short ClientHello body")
	}
	legacyVersion := uint16(body[0])<<8 | uint16(body[1])
	out.LegacyVersion = tlsVersionString(legacyVersion)
	cursor += 2 + 32

	sessionIDLen := int(body[cursor])
	cursor++
	if len(body) < cursor+sessionIDLen+2 {
		return out, fmt.Errorf("short ClientHello session id")
	}
	cursor += sessionIDLen

	cipherLen := int(body[cursor])<<8 | int(body[cursor+1])
	cursor += 2
	if len(body) < cursor+cipherLen+1 || cipherLen%2 != 0 {
		return out, fmt.Errorf("short ClientHello cipher suites")
	}
	ciphers := make([]uint16, 0, cipherLen/2)
	for i := 0; i < cipherLen; i += 2 {
		cipher := uint16(body[cursor+i])<<8 | uint16(body[cursor+i+1])
		ciphers = append(ciphers, cipher)
	}
	out.CipherSuites = uint16HexStringsWithoutGREASE(ciphers)
	cursor += cipherLen

	compressionLen := int(body[cursor])
	cursor++
	if len(body) < cursor+compressionLen {
		return out, fmt.Errorf("short ClientHello compression methods")
	}
	cursor += compressionLen
	if len(body) == cursor {
		out.HighestTLSVersion = out.LegacyVersion
		return out, nil
	}
	if len(body) < cursor+2 {
		return out, fmt.Errorf("short ClientHello extensions length")
	}
	extensionsLen := int(body[cursor])<<8 | int(body[cursor+1])
	cursor += 2
	if len(body) < cursor+extensionsLen {
		return out, fmt.Errorf("short ClientHello extensions")
	}

	extensionIDs := make([]uint16, 0)
	supportedVersions := make([]uint16, 0)
	extensionsEnd := cursor + extensionsLen
	for cursor+4 <= extensionsEnd {
		extID := uint16(body[cursor])<<8 | uint16(body[cursor+1])
		extLen := int(body[cursor+2])<<8 | int(body[cursor+3])
		cursor += 4
		if cursor+extLen > extensionsEnd {
			return out, fmt.Errorf("short ClientHello extension %d", extID)
		}
		extPayload := body[cursor : cursor+extLen]
		cursor += extLen
		extensionIDs = append(extensionIDs, extID)
		switch extID {
		case 0:
			out.ServerName = parseSNIExtension(extPayload)
		case 10:
			out.SupportedGroups = uint16HexStringsWithoutGREASE(parseUint16Vector(extPayload, 2))
		case 11:
			out.PointFormats = uint8HexStrings(parseUint8Vector(extPayload))
		case 13:
			out.SignatureSchemes = uint16HexStringsWithoutGREASE(parseUint16Vector(extPayload, 2))
		case 16:
			out.ALPNProtocols = parseALPNExtension(extPayload)
		case 43:
			supportedVersions = parseSupportedVersions(extPayload)
		}
	}
	out.Extensions = uint16HexStringsWithoutGREASE(extensionIDs)
	out.HighestTLSVersion = highestTLSVersionString(supportedVersions, legacyVersion)
	return out, nil
}

func parseSNIExtension(payload []byte) string {
	if len(payload) < 2 {
		return ""
	}
	listLen := int(payload[0])<<8 | int(payload[1])
	cursor := 2
	end := cursor + listLen
	if end > len(payload) {
		return ""
	}
	for cursor+3 <= end {
		nameType := payload[cursor]
		nameLen := int(payload[cursor+1])<<8 | int(payload[cursor+2])
		cursor += 3
		if cursor+nameLen > end {
			return ""
		}
		if nameType == 0 {
			return strings.ToLower(string(payload[cursor : cursor+nameLen]))
		}
		cursor += nameLen
	}
	return ""
}

func parseALPNExtension(payload []byte) []string {
	if len(payload) < 2 {
		return nil
	}
	listLen := int(payload[0])<<8 | int(payload[1])
	cursor := 2
	end := cursor + listLen
	if end > len(payload) {
		return nil
	}
	var protocols []string
	for cursor < end {
		protocolLen := int(payload[cursor])
		cursor++
		if cursor+protocolLen > end {
			return protocols
		}
		protocols = append(protocols, string(payload[cursor:cursor+protocolLen]))
		cursor += protocolLen
	}
	return protocols
}

func parseSupportedVersions(payload []byte) []uint16 {
	if len(payload) < 1 {
		return nil
	}
	listLen := int(payload[0])
	if len(payload) < 1+listLen {
		return nil
	}
	values := make([]uint16, 0, listLen/2)
	for i := 1; i+1 < 1+listLen; i += 2 {
		values = append(values, uint16(payload[i])<<8|uint16(payload[i+1]))
	}
	return values
}

func parseUint16Vector(payload []byte, lengthBytes int) []uint16 {
	if len(payload) < lengthBytes {
		return nil
	}
	listLen := 0
	for i := 0; i < lengthBytes; i++ {
		listLen = (listLen << 8) | int(payload[i])
	}
	cursor := lengthBytes
	end := cursor + listLen
	if end > len(payload) {
		return nil
	}
	values := make([]uint16, 0, listLen/2)
	for cursor+1 < end {
		values = append(values, uint16(payload[cursor])<<8|uint16(payload[cursor+1]))
		cursor += 2
	}
	return values
}

func parseUint8Vector(payload []byte) []uint8 {
	if len(payload) < 1 {
		return nil
	}
	listLen := int(payload[0])
	if len(payload) < 1+listLen {
		return nil
	}
	return append([]uint8(nil), payload[1:1+listLen]...)
}

func buildJA3Evidence(ch ClientHelloEvidence) FingerprintEvidence {
	ja3String := strings.Join([]string{
		ja3Version(ch.LegacyVersion),
		ja3Values(ch.CipherSuites),
		ja3Values(ch.Extensions),
		ja3Values(ch.SupportedGroups),
		ja3Values(ch.PointFormats),
	}, ",")
	sum := md5.Sum([]byte(ja3String))
	return FingerprintEvidence{
		String:    ja3String,
		Hash:      hex.EncodeToString(sum[:]),
		Algorithm: "ja3",
	}
}

func buildJA4Evidence(ch ClientHelloEvidence) FingerprintEvidence {
	tlsVersion := ja4Version(ch.HighestTLSVersion)
	if tlsVersion == "" {
		tlsVersion = ja4Version(ch.LegacyVersion)
	}
	sniMarker := "i"
	if ch.ServerName != "" {
		sniMarker = "d"
	}
	alpn := ja4ALPN(ch.ALPNProtocols)
	ciphers := hexStringsToDecimalStrings(ch.CipherSuites)
	extensions := hexStringsToDecimalStrings(ch.Extensions)
	sort.Strings(ciphers)
	sort.Strings(extensions)
	cipherHash := shortSHA256(strings.Join(ciphers, ","))
	extensionInput := strings.Join(extensions, ",") + "_" + strings.Join(hexStringsToDecimalStrings(ch.SignatureSchemes), ",")
	extensionHash := shortSHA256(extensionInput)
	value := fmt.Sprintf("t%s%s%02d%02d%s_%s_%s", tlsVersion, sniMarker, len(ciphers), len(extensions), alpn, cipherHash, extensionHash)
	return FingerprintEvidence{
		Value:     value,
		Algorithm: "ja4-clienthello-local-diagnostic",
	}
}

func ja3Version(version string) string {
	return fmt.Sprintf("%d", tlsVersionNumber(version))
}

func ja3Values(values []string) string {
	decimals := hexStringsToDecimalStrings(values)
	return strings.Join(decimals, "-")
}

func hexStringsToDecimalStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "0x")
		if value == "" {
			continue
		}
		var parsed uint16
		_, err := fmt.Sscanf(value, "%x", &parsed)
		if err == nil {
			out = append(out, fmt.Sprintf("%d", parsed))
		}
	}
	return out
}

func ja4Version(version string) string {
	switch tlsVersionNumber(version) {
	case 772:
		return "13"
	case 771:
		return "12"
	case 770:
		return "11"
	case 769:
		return "10"
	default:
		return ""
	}
}

func ja4ALPN(protocols []string) string {
	for _, protocol := range protocols {
		switch strings.ToLower(strings.TrimSpace(protocol)) {
		case "h2":
			return "h2"
		case "http/1.1":
			return "h1"
		}
	}
	if len(protocols) == 0 {
		return "00"
	}
	protocol := strings.ToLower(strings.TrimSpace(protocols[0]))
	if len(protocol) >= 2 {
		return protocol[:2]
	}
	return protocol + "0"
}

func tlsVersionNumber(version string) int {
	switch strings.ToUpper(strings.TrimSpace(version)) {
	case "TLS1.3":
		return 772
	case "TLS1.2":
		return 771
	case "TLS1.1":
		return 770
	case "TLS1.0":
		return 769
	default:
		return 0
	}
}

func highestTLSVersionString(versions []uint16, fallback uint16) string {
	highest := uint16(0)
	for _, version := range versions {
		if isGREASE(version) {
			continue
		}
		if version > highest {
			highest = version
		}
	}
	if highest == 0 {
		highest = fallback
	}
	return tlsVersionString(highest)
}

func tlsVersionString(version uint16) string {
	switch version {
	case 0x0304:
		return "TLS1.3"
	case 0x0303:
		return "TLS1.2"
	case 0x0302:
		return "TLS1.1"
	case 0x0301:
		return "TLS1.0"
	default:
		return fmt.Sprintf("0x%04x", version)
	}
}

func uint16HexStringsWithoutGREASE(values []uint16) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if isGREASE(value) {
			continue
		}
		out = append(out, fmt.Sprintf("0x%04x", value))
	}
	return out
}

func uint8HexStrings(values []uint8) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, fmt.Sprintf("0x%02x", value))
	}
	return out
}

func isGREASE(value uint16) bool {
	return value&0x0f0f == 0x0a0a && byte(value>>8) == byte(value)
}

func shortSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

func sha256HexLocal(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func selfSignedCertificate(host string) (tls.Certificate, error) {
	key, errKey := rsa.GenerateKey(rand.Reader, 2048)
	if errKey != nil {
		return tls.Certificate{}, errKey
	}
	serial, errSerial := rand.Int(rand.Reader, big.NewInt(1<<62))
	if errSerial != nil {
		return tls.Certificate{}, errSerial
	}
	template := x509.Certificate{
		SerialNumber: serial,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{host},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	certDER, errCert := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if errCert != nil {
		return tls.Certificate{}, errCert
	}
	keyDER := x509.MarshalPKCS1PrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER})
	return tls.X509KeyPair(certPEM, keyPEM)
}

func MarshalSyntheticProviderSNIEvidence(evidence *SyntheticProviderSNIEvidence) ([]byte, error) {
	return json.MarshalIndent(evidence, "", "  ")
}
