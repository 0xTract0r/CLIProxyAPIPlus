package executor

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type claudePairedWire struct {
	path    string
	body    []byte
	headers http.Header
}

func TestClaudeMessagesCountFirstPartyWireCompatibility(t *testing.T) {
	if !claudeCountSandboxChild(t) {
		return
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"api.anthropic.com"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certTemplate, certTemplate, &key.PublicKey, key)
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
	captures := make(chan claudePairedWire, 64)
	hellos := make(chan *tls.ClientHelloInfo, 64)
	sink := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Host != "api.anthropic.com" || req.Method != http.MethodPost || (req.URL.Path != "/v1/messages" && req.URL.Path != "/v1/messages/count_tokens") {
			t.Errorf("unexpected wire destination %s %s", req.Host, req.URL)
			http.Error(w, "denied", 403)
			return
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if req.ContentLength != int64(len(body)) {
			t.Errorf("wire content length=%d actual=%d", req.ContentLength, len(body))
		}
		captures <- claudePairedWire{req.URL.Path, bytes.Clone(body), req.Header.Clone()}
		w.Header().Set("anthropic-ratelimit-unified-5h-status", "allowed")
		if req.URL.Path == "/v1/messages/count_tokens" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens":34}`)
			return
		}
		a, b := gjson.GetBytes(body, "tools.0.name").String(), gjson.GetBytes(body, "tools.1.name").String()
		if !gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"msg-wire","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"tool_use","id":"u1","name":%q,"input":{}},{"type":"tool_use","id":"u2","name":%q,"input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":2}}`, a, b)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		wire := fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-wire\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5\",\"content\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"u1\",\"name\":%q,\"input\":{}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"Bash glob\\\"}\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", a)
		// Split inside JSON names and SSE delimiters; the executor must reassemble
		// transport fragments before restoring names, without rewriting argument text.
		for len(wire) > 0 {
			n := 3
			if len(wire) < n {
				n = len(wire)
			}
			_, _ = io.WriteString(w, wire[:n])
			wire = wire[n:]
			w.(http.Flusher).Flush()
		}
	}))
	sink.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{"http/1.1"}, GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) { hellos <- h; return nil, nil }}
	sink.StartTLS()
	defer sink.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect || req.Host != "api.anthropic.com:443" {
			http.Error(w, "unknown target denied", 403)
			return
		}
		upstream, err := net.Dial("tcp", sink.Listener.Addr().String())
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
	defer proxy.Close()
	payload := []byte(`{"model":"claude-opus-5","system":[{"type":"text","text":"private caller policy A","cache_control":{"type":"ephemeral"}},{"type":"text","text":"private caller policy B"}],"messages":[{"role":"user","content":[{"type":"text","text":"Bash and glob stay in prose"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}],"tools":[{"name":"Bash","description":"Bash prose stays","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}},{"name":"glob","input_schema":{"type":"object"}}],"prompt_cache_options":{"mode":"explicit"},"metadata":{"user_id":"caller-device"}}`)
	neutral := []string{"Use the available tools when needed to help with software engineering tasks.", "Keep responses concise and focused on the user's request.", "Prefer acting on the user's task over describing product-specific workflows."}
	for _, version := range []string{"2.1.211", "2.1.220"} {
		for _, tc := range []struct {
			name, mode           string
			oauth, native, cloak bool
		}{
			{"oauth-auto", "auto", true, false, true}, {"oauth-native-auto", "auto", true, true, false}, {"oauth-never", "never", true, false, false}, {"oauth-native-always", "always", true, true, true},
			{"key-auto", "auto", false, false, true}, {"key-native-auto", "auto", false, true, false}, {"key-never", "never", false, false, false}, {"key-always", "always", false, false, true},
		} {
			t.Run(version+"/"+tc.name, func(t *testing.T) {
				key := "synthetic-api-key"
				if tc.oauth {
					key = "sk-ant-oat-synthetic-wire-only"
				}
				a := &cliproxyauth.Auth{ID: version + "-" + tc.name, Provider: "claude", ProxyURL: proxy.URL, Attributes: map[string]string{"api_key": key, "cloak_mode": tc.mode}, Metadata: claudeOAuthTestMetadata()}
				cfg := &config.Config{}
				if version == "2.1.220" {
					cfg.ClaudeHeaderDefaults.PinSoftwareBaseline = true
					cfg.ClaudeHeaderDefaults.UserAgent = "claude-cli/" + version + " (external, cli)"
				}
				e := NewClaudeExecutor(cfg)
				ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ginCtx.Set("userApiKey", "synthetic-downstream-caller")
				ctx := context.WithValue(context.Background(), "gin", ginCtx)
				headers := http.Header{"User-Agent": []string{"caller-sdk/1.0"}}
				if tc.native {
					headers.Set("User-Agent", "claude-cli/"+version+" (external, cli)")
				}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: headers}
				req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: payload}
				counted, err := e.CountTokens(ctx, a, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				count := <-captures
				if count.path != "/v1/messages/count_tokens" {
					t.Fatal("count did not reach real first-party route")
				}
				if counted.Headers.Get("anthropic-ratelimit-unified-5h-status") != "allowed" {
					t.Fatal("count quota response header lost")
				}
				if gjson.GetBytes(count.body, "metadata").Exists() || bytes.Contains(count.body, []byte("x-anthropic-billing-header:")) {
					t.Fatalf("count gained Messages-only metadata/CCH: %s", count.body)
				}
				response, err := e.Execute(ctx, a, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				message := <-captures
				streamReq, streamOpts := req, opts
				streamReq.Payload, err = sjson.SetBytes(payload, "stream", true)
				if err != nil {
					t.Fatal(err)
				}
				streamOpts.Stream = true
				stream, err := e.ExecuteStream(ctx, a, streamReq, streamOpts)
				if err != nil {
					t.Fatal(err)
				}
				var downstream bytes.Buffer
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					downstream.Write(chunk.Payload)
				}
				streamed := <-captures
				if message.path != "/v1/messages" || streamed.path != "/v1/messages" {
					t.Fatal("serving route changed")
				}
				for _, body := range [][]byte{count.body, message.body, streamed.body} {
					if gjson.GetBytes(body, "model").String() != "claude-opus-5" || !bytes.Contains(body, []byte("aGVsbG8=")) || gjson.GetBytes(body, "tools.0.input_schema.properties.query.type").String() != "string" {
						t.Fatalf("model/image/tool schema lost: %s", body)
					}
					if tc.oauth && tc.cloak {
						for _, sentence := range neutral {
							if !strings.Contains(string(body), sentence) {
								t.Fatalf("effective neutral guidance lost: %s", body)
							}
						}
						if bytes.Contains(body, []byte("private caller policy")) {
							t.Fatal("OAuth caller policy leaked")
						}
					} else {
						for _, text := range []string{"private caller policy A", "private caller policy B"} {
							if !bytes.Contains(body, []byte(text)) {
								t.Fatalf("caller blocks lost: %s", body)
							}
						}
						if !bytes.Contains(body, []byte(`"cache_control"`)) {
							t.Fatal("caller cache block lost")
						}
					}
				}
				for i, want := range []string{"Bash", "glob"} {
					path := fmt.Sprintf("tools.%d.name", i)
					wire := gjson.GetBytes(count.body, path).String()
					if wire != gjson.GetBytes(message.body, path).String() || wire != gjson.GetBytes(streamed.body, path).String() {
						t.Fatal("Count and serving tool maps differ")
					}
					if (!tc.cloak || !tc.oauth) && wire != want {
						t.Fatalf("bypass name=%q want %q", wire, want)
					}
					if gjson.GetBytes(response.Payload, fmt.Sprintf("content.%d.name", i)).String() != want {
						t.Fatalf("nonstream name case changed: %s", response.Payload)
					}
				}
				if !strings.Contains(downstream.String(), `"name":"Bash"`) || !strings.Contains(downstream.String(), "Bash glob") {
					t.Fatalf("fragmented stream failed exact name/argument restoration: %s", downstream.String())
				}
				for _, wire := range []claudePairedWire{message, streamed} {
					ua := wire.headers.Get("User-Agent")
					billing := gjson.GetBytes(wire.body, "system.0.text").String()
					if tc.cloak || tc.native || tc.oauth {
						if !strings.HasPrefix(ua, "claude-cli/"+version+" ") {
							t.Fatalf("effective UA=%q want version %s", ua, version)
						}
					}
					if strings.HasPrefix(billing, "x-anthropic-billing-header:") && !strings.Contains(billing, "cc_version="+version+".") {
						t.Fatalf("billing/UA version mismatch: UA=%q billing=%q", ua, billing)
					}
					if tc.oauth {
						want := recomputeExpectedCCH(t, wire.body)
						if version == "2.1.220" {
							want = expectedCCH21220Fixture(t, wire.body)
						}
						if got := cchFromBody(t, wire.body); got != want {
							t.Fatalf("Messages CCH=%s want=%s for final UA %s", got, want, ua)
						}
						resigned, err := signAnthropicMessagesBodyForVersion(wire.body, version)
						if err != nil || !bytes.Equal(wire.body, resigned) {
							t.Fatal("Messages signing is not idempotent")
						}
					}
					t.Logf("stream=%v UA=%q billing=%q bytes=%d", gjson.GetBytes(wire.body, "stream").Bool(), ua, billing, len(wire.body))
				}
			})
		}
	}
	close(hellos)
	count := 0
	for h := range hellos {
		count++
		if h.ServerName != "api.anthropic.com" || !reflect.DeepEqual(h.SupportedProtos, []string{"http/1.1"}) {
			t.Fatalf("wrong TLS identity SNI=%q ALPN=%v", h.ServerName, h.SupportedProtos)
		}
	}
	if count == 0 {
		t.Fatal("no TLS handshake captured")
	}
	t.Logf("first-party Messages/Count/SSE mapped only to proxy=%s TLS sink=%s; handshakes=%d", proxy.Listener.Addr(), sink.Listener.Addr(), count)
}
