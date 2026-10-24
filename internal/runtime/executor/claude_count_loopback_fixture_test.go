package executor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const claudeCountLoopbackSandbox = `(version 1) (allow default) (deny network-outbound) (allow network-outbound (remote ip "localhost:*"))`

// This fixture never relies on a context RoundTripper override. The subprocess
// retains the production first-party URL, uTLS transport and certificate checks;
// an inherited OS sandbox denies every non-loopback socket, including fallbacks.
func claudeCountSandboxChild(t *testing.T) bool {
	t.Helper()
	if os.Getenv("CPA_CLAUDE_COUNT_LOOPBACK_CHILD") != "1" {
		if runtime.GOOS != "darwin" {
			t.Skip("first-party wire fixture requires the macOS loopback-only sandbox")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", claudeCountLoopbackSandbox, os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v", "-test.timeout=25s")
		cmd.Env = append(os.Environ(), "CPA_CLAUDE_COUNT_LOOPBACK_CHILD=1")
		output, err := cmd.CombinedOutput()
		t.Logf("isolated child:\n%s", output)
		if err != nil {
			t.Fatalf("loopback-only fixture failed: %v", err)
		}
		return false
	}
	// Verify the actual inherited restriction before touching the first-party URL.
	for _, address := range []string{"192.0.2.1:443", "198.51.100.1:8080"} {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if conn != nil {
			_ = conn.Close()
			t.Fatalf("sandbox allowed non-loopback dial %s", address)
		}
		if !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EACCES) {
			t.Fatalf("dial %s was not denied by OS: %v", address, err)
		}
		t.Logf("socket boundary denied %s: %v", address, err)
	}
	return true
}

func countClaudeFirstPartyThroughLoopbackTLS(t *testing.T, payload []byte) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"api.anthropic.com"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	debug := os.Getenv("GODEBUG")
	if debug != "" {
		debug += ","
	}
	t.Setenv("GODEBUG", debug+"x509usefallbackroots=1")
	x509.SetFallbackRoots(roots)
	type hello struct {
		sni     string
		alpn    []string
		ciphers []uint16
	}
	hellos := make(chan hello, 8)
	bodies := make(chan []byte, 8)
	var sinkRequests atomic.Int32
	sink := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinkRequests.Add(1)
		if r.Host != "api.anthropic.com" || r.URL.Path != "/v1/messages/count_tokens" || r.Method != http.MethodPost {
			t.Errorf("unexpected wire request %s %s host=%s", r.Method, r.URL, r.Host)
			w.WriteHeader(400)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		bodies <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":34}`)
	}))
	sink.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{"http/1.1"}, GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
		hellos <- hello{info.ServerName, append([]string(nil), info.SupportedProtos...), append([]uint16(nil), info.CipherSuites...)}
		return nil, nil
	}}
	sink.StartTLS()
	defer sink.Close()
	var deniedTargets atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "api.anthropic.com:443" {
			deniedTargets.Add(1)
			http.Error(w, "fixture proxy target denied", http.StatusForbidden)
			return
		}
		// This proxy has no DNS lookup or dial of r.Host: it only maps the one allowed
		// authority to the in-process TLS listener.
		upstream, err := net.Dial("tcp", sink.Listener.Addr().String())
		if err != nil {
			t.Error(err)
			http.Error(w, "sink unavailable", 502)
			return
		}
		downstream, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = upstream.Close()
			t.Error(err)
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, rw); _ = upstream.Close(); close(done) }()
		_, _ = io.Copy(downstream, upstream)
		_ = downstream.Close()
		<-done
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	probeTransport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer probeTransport.CloseIdleConnections()
	probe := &http.Client{Transport: probeTransport, Timeout: time.Second}
	if resp, err := probe.Get("https://unknown-count-fixture.invalid/"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("unknown proxy target accepted")
	}
	if deniedTargets.Load() != 1 || sinkRequests.Load() != 0 {
		t.Fatalf("unknown target reached sink: denied=%d sink=%d", deniedTargets.Load(), sinkRequests.Load())
	}
	auth := &cliproxyauth.Auth{ID: "count-local-fixture", Provider: "claude", ProxyURL: proxy.URL, Attributes: map[string]string{"api_key": "sk-ant-oat-synthetic-local-only"}, Metadata: claudeOAuthTestMetadata()}
	executor := NewClaudeExecutor(&config.Config{})
	req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: payload}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
	blockedCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, blockedErr := executor.CountTokens(cliproxyexecutor.WithRequestProxyURL(blockedCtx, "http://192.0.2.1:8080"), auth, req, opts)
	cancel()
	if blockedErr == nil || sinkRequests.Load() != 0 {
		t.Fatalf("wrong-proxy path escaped isolation: error=%v sink=%d", blockedErr, sinkRequests.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := executor.CountTokens(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Payload), "34") {
		t.Fatalf("unexpected count response %s", resp.Payload)
	}
	var wireHello hello
	select {
	case wireHello = <-hellos:
	default:
		t.Fatal("uTLS hello not captured")
	}
	wantCiphers := []uint16{4865, 4866, 4867, 49195, 49199, 49196, 49200, 52393, 52392, 49161, 49171, 49162, 49172, 156, 157, 47, 53}
	if wireHello.sni != "api.anthropic.com" || !reflect.DeepEqual(wireHello.alpn, []string{"http/1.1"}) || !reflect.DeepEqual(wireHello.ciphers, wantCiphers) {
		t.Fatalf("fork native ClientHello changed: %+v", wireHello)
	}
	if sinkRequests.Load() != 1 {
		t.Fatalf("sink requests = %d, want one", sinkRequests.Load())
	}
	t.Logf("logical host=api.anthropic.com:443; proxy socket=%s; TLS sink socket=%s; SNI=%s ALPN=%v ciphers=%v", proxyURL.Host, sink.Listener.Addr(), wireHello.sni, wireHello.alpn, wireHello.ciphers)
	select {
	case body := <-bodies:
		return body
	default:
		t.Fatal("count body not captured")
	}
	return nil
}

func countReview216Official(t *testing.T, payload []byte, cfg *config.Config, auth *cliproxyauth.Auth, opts cliproxyexecutor.Options) ([]byte, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"api.anthropic.com"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	debug := os.Getenv("GODEBUG")
	if debug != "" {
		debug += ","
	}
	t.Setenv("GODEBUG", debug+"x509usefallbackroots=1")
	x509.SetFallbackRoots(roots)
	type hello struct {
		sni     string
		alpn    []string
		ciphers []uint16
	}
	hellos := make(chan hello, 8)
	bodies := make(chan []byte, 8)
	uas := make(chan string, 8)
	var sinkRequests atomic.Int32
	sink := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinkRequests.Add(1)
		if r.Host != "api.anthropic.com" || r.URL.Path != "/v1/messages/count_tokens" || r.Method != http.MethodPost {
			t.Errorf("unexpected wire request %s %s host=%s", r.Method, r.URL, r.Host)
			w.WriteHeader(400)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		bodies <- body
		uas <- r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":34}`)
	}))
	sink.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{"http/1.1"}, GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
		hellos <- hello{info.ServerName, append([]string(nil), info.SupportedProtos...), append([]uint16(nil), info.CipherSuites...)}
		return nil, nil
	}}
	sink.StartTLS()
	defer sink.Close()
	var deniedTargets atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "api.anthropic.com:443" {
			deniedTargets.Add(1)
			http.Error(w, "fixture proxy target denied", http.StatusForbidden)
			return
		}
		// This proxy has no DNS lookup or dial of r.Host: it only maps the one allowed
		// authority to the in-process TLS listener.
		upstream, err := net.Dial("tcp", sink.Listener.Addr().String())
		if err != nil {
			t.Error(err)
			http.Error(w, "sink unavailable", 502)
			return
		}
		downstream, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = upstream.Close()
			t.Error(err)
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, rw); _ = upstream.Close(); close(done) }()
		_, _ = io.Copy(downstream, upstream)
		_ = downstream.Close()
		<-done
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	probeTransport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	defer probeTransport.CloseIdleConnections()
	probe := &http.Client{Transport: probeTransport, Timeout: time.Second}
	if resp, err := probe.Get("https://unknown-count-fixture.invalid/"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("unknown proxy target accepted")
	}
	if deniedTargets.Load() != 1 || sinkRequests.Load() != 0 {
		t.Fatalf("unknown target reached sink: denied=%d sink=%d", deniedTargets.Load(), sinkRequests.Load())
	}
	auth.ProxyURL = proxy.URL
	executor := NewClaudeExecutor(cfg)
	req := cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: payload}
	blockedCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, blockedErr := executor.CountTokens(cliproxyexecutor.WithRequestProxyURL(blockedCtx, "http://192.0.2.1:8080"), auth, req, opts)
	cancel()
	if blockedErr == nil || sinkRequests.Load() != 0 {
		t.Fatalf("wrong-proxy path escaped isolation: error=%v sink=%d", blockedErr, sinkRequests.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := executor.CountTokens(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Payload), "34") {
		t.Fatalf("unexpected count response %s", resp.Payload)
	}
	var wireHello hello
	select {
	case wireHello = <-hellos:
	default:
		t.Fatal("uTLS hello not captured")
	}
	wantCiphers := []uint16{4865, 4866, 4867, 49195, 49199, 49196, 49200, 52393, 52392, 49161, 49171, 49162, 49172, 156, 157, 47, 53}
	if wireHello.sni != "api.anthropic.com" || !reflect.DeepEqual(wireHello.alpn, []string{"http/1.1"}) || !reflect.DeepEqual(wireHello.ciphers, wantCiphers) {
		t.Fatalf("fork native ClientHello changed: %+v", wireHello)
	}
	if sinkRequests.Load() != 1 {
		t.Fatalf("sink requests = %d, want one", sinkRequests.Load())
	}
	t.Logf("logical host=api.anthropic.com:443; proxy socket=%s; TLS sink socket=%s; SNI=%s ALPN=%v ciphers=%v", proxyURL.Host, sink.Listener.Addr(), wireHello.sni, wireHello.alpn, wireHello.ciphers)
	select {
	case body := <-bodies:
		t.Logf("216 sink request path=/v1/messages/count_tokens nonce=%s bytes=%d", t.Name(), len(body))
		return body, <-uas
	default:
		t.Fatal("count body not captured")
	}
	return nil, ""
}
