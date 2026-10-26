package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// API-key bypass requests can carry a caller-owned CCH without opting into the
// CLI fingerprint profile. Fork identity rewrites must re-sign that existing
// CCH, while disabled rewrites and unsigned requests retain their boundaries.
func TestClaudeAPIKeyIdentityRewriteResignsExistingCCH(t *testing.T) {
	for _, version := range []string{"2.1.211", "2.1.220"} {
		for _, mode := range []string{"auto", "never"} {
			for _, streaming := range []bool{false, true} {
				for _, tc := range []struct {
					name             string
					fold, align, cch bool
				}{
					{"fold", true, false, true}, {"align", false, true, true}, {"both", true, true, true},
					{"unchanged", false, false, true}, {"unsigned", true, true, false},
				} {
					t.Run(fmt.Sprintf("%s/%s/stream=%v/%s", version, mode, streaming, tc.name), func(t *testing.T) {
						resetClaudeDeviceProfileCache()
						captured := make(chan claudePairedWire, 1)
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							body, err := io.ReadAll(r.Body)
							if err != nil {
								t.Error(err)
								return
							}
							if r.URL.Path != "/v1/messages" || r.ContentLength != int64(len(body)) {
								t.Errorf("wire path/length mismatch: %s/%d/%d", r.URL.Path, r.ContentLength, len(body))
							}
							captured <- claudePairedWire{path: r.URL.Path, body: bytes.Clone(body), headers: r.Header.Clone()}
							if streaming {
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-local\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":0}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
							} else {
								w.Header().Set("Content-Type", "application/json")
								_, _ = io.WriteString(w, `{"id":"msg-local","type":"message","role":"assistant","model":"claude-opus-5","content":[],"usage":{"input_tokens":1,"output_tokens":0},"stop_reason":"end_turn"}`)
							}
						}))
						defer server.Close()
						cfg := &config.Config{Claude: config.ClaudeConfig{NormalizeSdkCliEntrypoint: &tc.fold, AlignRealPathBillingVersion: &tc.align}, ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{PinSoftwareBaseline: true, UserAgent: "claude-cli/" + version + " (external, cli)"}}
						a := &cliproxyauth.Auth{ID: t.Name(), Provider: "claude", ProxyURL: "direct", Attributes: map[string]string{"api_key": "synthetic-api-key", "base_url": server.URL, "cloak_mode": mode}}
						bodyVersion := version
						if tc.align {
							bodyVersion = "2.1.158"
						}
						body := billingBodyWithVersion(t, bodyVersion, "old", "11111")
						header := gjson.GetBytes(body, "system.0.text").String()
						if tc.fold {
							header = strings.Replace(header, "cc_entrypoint=cli;", "cc_entrypoint=sdk-cli;", 1)
						}
						if !tc.cch {
							header = strings.Replace(header, " cch=11111;", "", 1)
						}
						var err error
						body, err = sjson.SetBytes(body, "system.0.text", header)
						if err != nil {
							t.Fatal(err)
						}
						body, err = sjson.SetBytes(body, "stream", streaming)
						if err != nil {
							t.Fatal(err)
						}
						e := NewClaudeExecutor(cfg)
						req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: body}
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: http.Header{"User-Agent": []string{"claude-cli/" + version + " (external, cli)"}}, Stream: streaming}
						if streaming {
							result, err := e.ExecuteStream(context.Background(), a, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
							}
						} else {
							if _, err := e.Execute(context.Background(), a, req, opts); err != nil {
								t.Fatal(err)
							}
						}
						wire := <-captured
						uaVersion, _ := helps.ClaudeVersionFromUserAgent(wire.headers.Get("User-Agent"))
						if uaVersion != version {
							t.Fatalf("final UA version %q want %q", uaVersion, version)
						}
						gotHeader := gjson.GetBytes(wire.body, "system.0.text").String()
						if tc.fold && !strings.Contains(gotHeader, "cc_entrypoint=cli;") {
							t.Fatal("entrypoint was not folded")
						}
						if tc.align && ccVersionSegment(t, wire.body) != version {
							t.Fatal("billing version was not aligned")
						}
						if !tc.cch {
							if strings.Contains(gotHeader, "cch=") {
								t.Fatalf("unsigned API-key request gained CCH: %s", gotHeader)
							}
							return
						}
						got := cchFromBody(t, wire.body)
						if !tc.fold && !tc.align {
							if got != "11111" {
								t.Fatalf("unchanged caller CCH was rewritten: %s", got)
							}
							return
						}
						want := recomputeExpectedCCH(t, wire.body)
						if version == "2.1.220" {
							want = expectedCCH21220Fixture(t, wire.body)
						}
						if got != want {
							t.Fatalf("rewritten API-key body retained CCH=%s; want final-UA/final-byte CCH=%s", got, want)
						}
					})
				}
			}
		}
	}
}
