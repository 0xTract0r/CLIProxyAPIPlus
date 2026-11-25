package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/klauspost/compress/zstd"
	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryCodexHTTPParity(t *testing.T) {
	fixture := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": "https://provider.invalid"}}
	e := NewCodexExecutor(&config.Config{})
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(`{"model":"gpt-5","input":[{"type":"web_search_call","id":"ws_fixture","status":"completed"}],"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}],"max_output_tokens":32}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	rt := ordinaryCodexTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if ojson.GetBytesNoCopy(body, "tools.0.name").String() != "weather" || ojson.GetBytesNoCopy(body, "tools.1.type").String() != "web_search" {
			t.Fatalf("tools lost: %s", body)
		}
		var payload bytes.Buffer
		z := gzip.NewWriter(&payload)
		_, _ = io.WriteString(z, `data: {"type":"response.completed","response":{"id":"resp_fixture","object":"response","status":"completed","model":"gpt-5","output":[{"type":"message","id":"msg_fixture","role":"assistant","content":[{"type":"output_text","text":"compressed"}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`+"\n\n")
		if err = z.Close(); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {"gzip"}, "Content-Type": {"text/event-stream"}}, Body: io.NopCloser(&payload), Request: r}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", rt)
	out, err := e.Execute(ctx, fixture, req, opts)
	if err != nil || !strings.Contains(string(out.Payload), "compressed") {
		t.Fatalf("nonstream response=%s err=%v", out.Payload, err)
	}
	outStream, err := e.ExecuteStream(ctx, fixture, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var wire strings.Builder
	for chunk := range outStream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		wire.Write(chunk.Payload)
	}
	if !strings.Contains(wire.String(), "compressed") {
		t.Fatalf("stream=%s", wire.String())
	}
}

func TestOrdinaryCodexRefreshGrantCAS(t *testing.T) {
	for _, name := range []string{"success", "same account different grant", "in-flight switch", "provider refusal"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credential.json")
			record := map[string]any{"refresh_token": "refresh-fixture", "access_token": "access-fixture", "account_id": "account-fixture", "email": "fixture@invalid", "note": "preserve"}
			write := func(m map[string]any) {
				b, _ := json.Marshal(m)
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(record)
			auth := &cliproxyauth.Auth{Attributes: map[string]string{cliproxyauth.AttributePath: path}, Metadata: map[string]any{"refresh_token": "refresh-fixture", "access_token": "access-fixture", "account_id": "account-fixture", "email": "fixture@invalid"}}
			if name == "same account different grant" {
				record["refresh_token"] = "other-grant"
				write(record)
			}
			calls := 0
			denial := errors.New("authorization refused")
			grant := ordinaryCodexGrantFunc(func(_ context.Context, token string) (*codexauth.CodexTokenData, error) {
				calls++
				if token != "refresh-fixture" {
					t.Fatal("unexpected refresh grant")
				}
				if name == "provider refusal" {
					return nil, denial
				}
				if name == "in-flight switch" {
					record["refresh_token"] = "other-grant"
					write(record)
				}
				return &codexauth.CodexTokenData{AccessToken: "access-rotated", RefreshToken: "refresh-rotated", AccountID: "account-fixture"}, nil
			})
			ctx := context.WithValue(t.Context(), ordinaryCodexGrantKey{}, grant)
			fresh, err := NewCodexExecutor(&config.Config{}).Refresh(ctx, auth)
			actual, readErr := readOrdinaryCodexCredential(path)
			if readErr != nil || actual["note"] != "preserve" || actual["email"] != "fixture@invalid" || auth.Metadata["access_token"] != "access-fixture" {
				t.Fatalf("readErr=%v", readErr)
			}
			wantCalls := 1
			if name == "same account different grant" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d want=%d", calls, wantCalls)
			}
			switch name {
			case "success":
				if err != nil || fresh.CodexRefreshPatch == nil || actual["access_token"] != "access-rotated" || actual["refresh_token"] != "refresh-rotated" {
					t.Fatalf("err=%v", err)
				}
			case "provider refusal":
				if !errors.Is(err, denial) || fresh != nil || actual["access_token"] != "access-fixture" {
					t.Fatalf("err=%v", err)
				}
			default:
				var conflict statusErr
				if !errors.As(err, &conflict) || conflict.StatusCode() != 409 || fresh != nil || actual["refresh_token"] != "other-grant" || actual["access_token"] != "access-fixture" {
					t.Fatalf("err=%v", err)
				}
			}
		})
	}
}

func TestOrdinaryCodexOutboundCompression(t *testing.T) {
	cfg := &config.Config{}
	cfg.Codex.UpstreamRequestCompression = true
	e := NewCodexExecutor(cfg)
	a := &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "oauth-fixture"}}
	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(`{"model":"gpt-5","input":"` + strings.Repeat("a", cfg.Codex.RequestCompressionMinBytes()+1) + `"}`)}
	rt := ordinaryCodexTestRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "chatgpt.com" || r.Header.Get("Content-Encoding") != "zstd" || r.Header.Get("Authorization") != "Bearer oauth-fixture" {
			t.Fatalf("request %s encoding=%q", r.URL, r.Header.Get("Content-Encoding"))
		}
		dec, err := zstd.NewReader(r.Body, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(32<<20))
		if err != nil {
			t.Fatal(err)
		}
		defer dec.Close()
		raw, err := io.ReadAll(dec)
		if err != nil || !ojson.GetBytesNoCopy(raw, "stream").Bool() || !strings.Contains(string(raw), strings.Repeat("a", 100)) {
			t.Fatalf("invalid decoded body: %v", err)
		}
		return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"fixture"}}`)), Request: r}, nil
	})
	_, err := e.Execute(context.WithValue(t.Context(), "cliproxy.roundtripper", rt), a, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	var status statusErr
	if !errors.As(err, &status) || status.StatusCode() != 429 {
		t.Fatalf("err=%v", err)
	}
}
