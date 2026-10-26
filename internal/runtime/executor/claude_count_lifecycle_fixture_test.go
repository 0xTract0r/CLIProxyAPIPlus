package executor

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// newClaudeCountLifecycleSink 仅供独立 OS 沙盒子进程使用；保留生产 first-party
// URL、证书验证与 uTLS，把唯一允许的 CONNECT 目标映射到本进程 TLS listener。
func newClaudeCountLifecycleSink(t *testing.T, handler http.Handler) string {
	t.Helper()
	if os.Getenv("CPA_CLAUDE_COUNT_LOOPBACK_CHILD") != "1" {
		t.Fatal("first-party fixture requires claudeCountSandboxChild")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(63), DNSNames: []string{"api.anthropic.com"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
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
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",x509usefallbackroots=1")
	x509.SetFallbackRoots(roots)
	hellos := make(chan *tls.ClientHelloInfo, 4)
	var calls atomic.Int32
	sink := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Host != "api.anthropic.com" || req.Method != http.MethodPost || req.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("unexpected count route %s %s %s", req.Method, req.Host, req.URL)
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		calls.Add(1)
		handler.ServeHTTP(w, req)
	}))
	sink.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{"http/1.1"}, GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) { hellos <- h; return nil, nil }}
	sink.StartTLS()
	t.Cleanup(sink.Close)
	addr, ok := sink.Listener.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		t.Fatal("TLS sink is not loopback")
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect || req.Host != "api.anthropic.com:443" {
			http.Error(w, "unknown target denied", http.StatusForbidden)
			return
		}
		upstream, err := net.Dial("tcp", addr.String())
		if err != nil {
			t.Error(err)
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
	t.Cleanup(proxy.Close)
	t.Cleanup(func() {
		if calls.Load() != 1 {
			t.Errorf("count sink requests=%d, want one", calls.Load())
		}
		select {
		case h := <-hellos:
			want := []uint16{4865, 4866, 4867, 49195, 49199, 49196, 49200, 52393, 52392, 49161, 49171, 49162, 49172, 156, 157, 47, 53}
			if h.ServerName != "api.anthropic.com" || !reflect.DeepEqual(h.SupportedProtos, []string{"http/1.1"}) || !reflect.DeepEqual(h.CipherSuites, want) {
				t.Errorf("fork uTLS fingerprint changed: SNI=%s ALPN=%v ciphers=%v", h.ServerName, h.SupportedProtos, h.CipherSuites)
			}
			t.Logf("logical=api.anthropic.com:443 proxy=%s sink=%s SNI=%s ALPN=%v", proxy.URL, addr, h.ServerName, h.SupportedProtos)
		default:
			t.Error("no uTLS ClientHello captured")
		}
	})
	return proxy.URL
}
