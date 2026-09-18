package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type claudeAttemptTestGate struct {
	mu        sync.Mutex
	infos     []cliproxyexecutor.HTTPAttemptInfo
	permits   []*claudeAttemptTestPermit
	before    func(int) error
	cancel    context.CancelFunc
	markErr   error
	finishErr error
}

type claudeAttemptTestPermit struct {
	mu                        sync.Mutex
	sent, cancelled, finished int
	result                    cliproxyexecutor.HTTPAttemptResult
	markErr                   error
	finishErr                 error
	settled                   chan struct{}
}

func (g *claudeAttemptTestGate) Before(_ context.Context, info cliproxyexecutor.HTTPAttemptInfo) (cliproxyexecutor.HTTPAttemptPermit, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.infos = append(g.infos, info)
	if g.before != nil {
		if err := g.before(len(g.infos)); err != nil {
			return nil, err
		}
	}
	p := &claudeAttemptTestPermit{markErr: g.markErr, finishErr: g.finishErr, settled: make(chan struct{}, 4)}
	g.permits = append(g.permits, p)
	if g.cancel != nil {
		g.cancel()
	}
	return p, nil
}
func (p *claudeAttemptTestPermit) MarkSent(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent++
	return p.markErr
}
func (p *claudeAttemptTestPermit) CancelUnsent(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	p.cancelled++
	return nil
}
func (p *claudeAttemptTestPermit) Finish(ctx context.Context, result cliproxyexecutor.HTTPAttemptResult) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	p.finished++
	p.result = result
	p.settled <- struct{}{}
	return p.finishErr
}
func (p *claudeAttemptTestPermit) snapshot() (int, int, int, cliproxyexecutor.HTTPAttemptResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent, p.cancelled, p.finished, p.result
}

type claudeAttemptRoundTripFunc func(*http.Request) (*http.Response, error)

func (f claudeAttemptRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const claudeAttemptRequestJSON = `{"model":"claude-sonnet-4-6","max_tokens":100,"system":"Review a Go concurrency gate.","messages":[{"role":"user","content":"Inspect cancellation and counters."}],"tools":[]}`
const claudeAttemptResponseJSON = `{"type":"message","usage":{"input_tokens":10,"output_tokens":4,"cache_creation_input_tokens":3,"cache_read_input_tokens":1000}}`
const claudeAttemptStreamUsage = "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0,\"cache_creation_input_tokens\":3,\"cache_read_input_tokens\":1000}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\n\ndata: {\"type\":\"message_stop\"}\n\n"

func claudeAttemptRequest(t *testing.T, ctx context.Context, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(claudeAttemptRequestJSON))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestClaudeHTTPAttemptTransportRetry(t *testing.T) {
	for _, rejectRetry := range []bool{false, true} {
		t.Run(map[bool]string{false: "three-attempts", true: "later-denial"}[rejectRetry], func(t *testing.T) {
			gate := &claudeAttemptTestGate{}
			blocked := errors.New("synthetic admission denial")
			if rejectRetry {
				gate.before = func(n int) error {
					if n > 1 {
						return blocked
					}
					return nil
				}
			}
			ctx := cliproxyexecutor.WithHTTPAttemptGate(context.Background(), gate)
			calls := 0
			client := &http.Client{Transport: claudeAttemptRoundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if calls < 3 {
					return nil, io.ErrUnexpectedEOF
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(claudeAttemptResponseJSON))}, nil
			})}
			resp, err := doClaudeHTTPWithTransportRetry(ctx, client, claudeAttemptRequest(t, ctx, "http://localhost/v1/messages"))
			if rejectRetry {
				var gateErr *cliproxyexecutor.HTTPAttemptGateError
				if !errors.As(err, &gateErr) || !errors.Is(err, blocked) || !errors.Is(gateErr.Previous, io.ErrUnexpectedEOF) || calls != 1 {
					t.Fatalf("retry denial lost origin/retried: calls=%d err=%v", calls, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if string(body) != claudeAttemptResponseJSON || calls != 3 {
					t.Fatal("response mutated or retries skipped")
				}
			}
			for i, p := range gate.permits {
				sent, cancelled, finished, result := p.snapshot()
				if sent != 1 || cancelled != 0 || finished != 1 {
					t.Fatalf("attempt %d lifecycle %d/%d/%d", i, sent, cancelled, finished)
				}
				if i < 2 && !rejectRetry && result.Complete {
					t.Fatal("connection failure completed usage")
				}
			}
		})
	}
}

func TestClaudeHTTPAttemptLocalDenials(t *testing.T) {
	for _, kind := range []string{"before", "cancel", "mark"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			gate := &claudeAttemptTestGate{}
			if kind == "before" {
				gate.before = func(int) error { return io.ErrUnexpectedEOF }
			}
			if kind == "cancel" {
				gate.cancel = cancel
			}
			if kind == "mark" {
				gate.markErr = io.ErrUnexpectedEOF
			}
			ctx = cliproxyexecutor.WithHTTPAttemptGate(ctx, gate)
			calls := 0
			client := &http.Client{Transport: claudeAttemptRoundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, io.ErrUnexpectedEOF })}
			_, err := doClaudeHTTPWithTransportRetry(ctx, client, claudeAttemptRequest(t, ctx, "http://localhost/v1/messages"))
			if !cliproxyexecutor.IsHTTPAttemptGateError(err) || calls != 0 || len(gate.infos) != 1 {
				t.Fatalf("local refusal sent or retried: %d %v", calls, err)
			}
			if len(gate.permits) > 0 {
				p := gate.permits[0]
				_, cancelled, finished, _ := p.snapshot()
				if cancelled != 1 || finished != 0 {
					t.Fatal("unsent reservation not refunded exactly once")
				}
			}
		})
	}
}

func TestClaudeHTTPAttemptRedirectNilCompatibility(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		if r.URL.Path == "/v1/messages" {
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, claudeAttemptResponseJSON)
	}))
	defer server.Close()
	client := server.Client()
	for _, enabled := range []bool{false, true} {
		sends.Store(0)
		gate := &claudeAttemptTestGate{}
		ctx := context.Background()
		if enabled {
			ctx = cliproxyexecutor.WithHTTPAttemptGate(ctx, gate)
		} else {
			ctx = cliproxyexecutor.WithHTTPAttemptGate(ctx, nil)
		}
		resp, err := doClaudeHTTPWithTransportRetry(ctx, client, claudeAttemptRequest(t, ctx, server.URL+"/v1/messages"))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		want := int32(2)
		if enabled {
			want = 1
		}
		if sends.Load() != want || client.CheckRedirect != nil {
			t.Fatal("redirect escaped gate or changed shared client")
		}
	}
}

func TestClaudeHTTPAttemptUsageAndClose(t *testing.T) {
	cases := []struct {
		name, body, content, path string
		status                    int
		complete                  bool
		tokens                    int64
	}{
		{"json", claudeAttemptResponseJSON, "application/json", "/v1/messages", 200, true, 17},
		{"split-cumulative", claudeAttemptStreamUsage, "text/event-stream", "/v1/messages", 200, true, 17},
		{"initial-output-only", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"cache_creation_input_tokens\":3}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "text/event-stream", "/v1/messages", 200, false, 14},
		{"final-delta-output-missing", strings.Replace(claudeAttemptStreamUsage, "\"usage\":{\"output_tokens\":4}", "\"delta\":{\"stop_reason\":\"end_turn\"}", 1), "text/event-stream", "/v1/messages", 200, false, 15},
		{"missing", `{"type":"message"}`, "application/json", "/v1/messages", 200, false, 0},
		{"partial", strings.Split(claudeAttemptStreamUsage, "data: {\"type\":\"message_stop\"}")[0], "text/event-stream", "/v1/messages", 200, false, 17},
		{"error", claudeAttemptStreamUsage + "data: {\"type\":\"error\"}\n\n", "text/event-stream", "/v1/messages", 200, false, 17},
		{"status-error", claudeAttemptResponseJSON, "application/json", "/v1/messages", 500, false, 17},
		{"count", `{"input_tokens":12000}`, "application/json", "/v1/messages/count_tokens", 200, true, 0},
		{"count-missing", `{}`, "application/json", "/v1/messages/count_tokens", 200, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := &claudeAttemptTestGate{}
			ctx := cliproxyexecutor.WithHTTPAttemptGate(context.Background(), gate)
			client := &http.Client{Transport: claudeAttemptRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{tc.content}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			resp, err := doClaudeHTTPWithTransportRetry(ctx, client, claudeAttemptRequest(t, ctx, "http://localhost"+tc.path))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			_ = resp.Body.Close()
			_, _, finished, result := gate.permits[0].snapshot()
			if string(body) != tc.body || finished != 1 || result.Complete != tc.complete || result.SchedulerTokens != tc.tokens {
				t.Fatalf("usage/bytes: finished=%d result=%+v", finished, result)
			}
		})
	}
}

func TestClaudeHTTPAttemptDecodedOwnership(t *testing.T) {
	for _, abandon := range []bool{false, true} {
		t.Run(map[bool]string{false: "gzip", true: "claim-abandoned"}[abandon], func(t *testing.T) {
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			_, _ = writer.Write([]byte(claudeAttemptResponseJSON))
			_ = writer.Close()
			gate := &claudeAttemptTestGate{}
			ctx := cliproxyexecutor.WithHTTPAttemptGate(context.Background(), gate)
			client := &http.Client{Transport: claudeAttemptRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}, "Content-Encoding": []string{"gzip"}}, Body: io.NopCloser(bytes.NewReader(compressed.Bytes()))}, nil
			})}
			resp, err := doClaudeHTTPWithTransportRetry(ctx, client, claudeAttemptRequest(t, ctx, "http://localhost/v1/messages"))
			if err != nil {
				t.Fatal(err)
			}
			attempt := helps.ClaimClaudeAttemptResponse(resp)
			if abandon {
				attempt.Abandon()
			} else {
				decoded, err := gzip.NewReader(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				observed := attempt.ObserveDecoded(decoded)
				data, err := io.ReadAll(observed)
				if err != nil || string(data) != claudeAttemptResponseJSON {
					t.Fatal("decoded bytes changed", err)
				}
				_ = observed.Close()
				_ = resp.Body.Close()
			}
			_, _, finished, result := gate.permits[0].snapshot()
			if finished != 1 || result.Complete == abandon {
				t.Fatalf("claim leaked or ended before decoded usage: %d %+v", finished, result)
			}
		})
	}
}

func TestClaudeHTTPAttemptCancellationRace(t *testing.T) {
	gate := &claudeAttemptTestGate{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = cliproxyexecutor.WithHTTPAttemptGate(ctx, gate)
	reader, writer := io.Pipe()
	defer writer.Close()
	client := &http.Client{Transport: claudeAttemptRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}, nil
	})}
	resp, err := doClaudeHTTPWithTransportRetry(ctx, client, claudeAttemptRequest(t, ctx, "http://localhost/v1/messages"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(io.Discard, resp.Body) }()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); cancel(); _ = resp.Body.Close() }()
	}
	wg.Wait()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel left a blocked body read")
	}
	_, _, finished, result := gate.permits[0].snapshot()
	if finished != 1 || result.Complete {
		t.Fatalf("cancel/close settled incorrectly: %d %+v", finished, result)
	}
}

func TestClaudeHTTPAttemptPreparedEstimate(t *testing.T) {
	for _, tc := range []struct {
		name, body, path string
		known            bool
	}{
		{"text", claudeAttemptRequestJSON, "/v1/messages", true},
		{"media", `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"synthetic"}}]}]}`, "/v1/messages", false},
		{"tool-reference", `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"tool_reference","tool_name":"inspect"}]}]}]}`, "/v1/messages", false},
		{"unknown-top", `{"model":"claude","max_tokens":100,"mcp_servers":[],"messages":[{"role":"user","content":"Inspect concurrency"}]}`, "/v1/messages", false},
		{"missing-output-bound", `{"model":"claude","messages":[{"role":"user","content":"Inspect concurrency"}]}`, "/v1/messages", false},
		{"null-text", `{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":null}]}]}`, "/v1/messages", false},
		{"custom-tools", `{"model":"claude","max_tokens":100,"system":"Check code","tools":[{"name":"inspect","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"inspect","input":{"path":"file.go","type":"business-value"},"caller":{"type":"direct"}}]}]}`, "/v1/messages", true},
		{"server-tool", `{"model":"claude","max_tokens":100,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"Inspect concurrency"}]}`, "/v1/messages", false},
		{"count", `{"model":"claude","messages":[{"role":"user","content":"Inspect concurrency"}]}`, "/v1/messages/count_tokens", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := &claudeAttemptTestGate{}
			ctx := cliproxyexecutor.WithHTTPAttemptGate(context.Background(), gate)
			client := &http.Client{Transport: claudeAttemptRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				data, err := io.ReadAll(r.Body)
				if err != nil || string(data) != tc.body {
					t.Error("prepared body mutated", err)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(claudeAttemptResponseJSON))}, nil
			})}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost"+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := doClaudeHTTPWithTransportRetry(ctx, client, req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			info := gate.infos[0]
			if info.EstimateKnown != tc.known {
				t.Fatalf("estimate knowledge: %+v", info)
			}
			if info.CountOnly {
				if info.EstimatedTokens != 0 {
					t.Fatal("CountTokens was treated as generation")
				}
			} else if tc.known && info.EstimatedTokens < int64(len(tc.body))+100 {
				t.Fatalf("estimate omitted prepared input/output: %+v", info)
			}
		})
	}
}

func TestClaudeHTTPAttemptRealExecutorEntries(t *testing.T) {
	for _, kind := range []string{"execute", "stream", "count", "execute-decode-failure", "stream-decode-failure", "count-decode-failure"} {
		t.Run(kind, func(t *testing.T) {
			var sends atomic.Int32
			var preparedBytes atomic.Int64
			invalidCompression := strings.HasSuffix(kind, "decode-failure")
			entry := strings.Split(kind, "-")[0]
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				body, readErr := io.ReadAll(r.Body)
				if readErr != nil {
					t.Error(readErr)
				}
				preparedBytes.Store(int64(len(body)))
				if invalidCompression {
					w.Header().Set("Content-Encoding", "gzip")
					_, _ = io.WriteString(w, "invalid-compressed-body")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if entry == "count" {
					if r.URL.Path != "/v1/messages/count_tokens" {
						t.Error("wrong Count endpoint")
					}
					_, _ = io.WriteString(w, `{"input_tokens":100}`)
					return
				}
				if entry == "stream" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, claudeAttemptStreamUsage)
					return
				}
				_, _ = io.WriteString(w, claudeAttemptResponseJSON)
			}))
			defer server.Close()
			gate := &claudeAttemptTestGate{}
			ctx := cliproxyexecutor.WithHTTPAttemptGate(context.Background(), gate)
			exec := NewClaudeExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{ID: "synthetic-http-attempt", Provider: "claude", ProxyURL: "direct", Attributes: map[string]string{"api_key": "key-123", "base_url": server.URL}}
			req := cliproxyexecutor.Request{Model: "claude-3-5-sonnet-20241022", Payload: []byte(claudeAttemptRequestJSON)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}
			var err error
			switch entry {
			case "stream":
				var stream *cliproxyexecutor.StreamResult
				stream, err = exec.ExecuteStream(ctx, auth, req, opts)
				if err == nil {
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				}
			case "count":
				_, err = exec.CountTokens(ctx, auth, req, opts)
			default:
				_, err = exec.Execute(ctx, auth, req, opts)
			}
			if !invalidCompression && err != nil {
				t.Fatal(err)
			}
			if invalidCompression && err == nil {
				t.Fatal("corrupt compression succeeded")
			}
			if sends.Load() != 1 || len(gate.permits) != 1 {
				t.Fatalf("entry bypassed hook: sends=%d permits=%d", sends.Load(), len(gate.permits))
			}
			if info := gate.infos[0]; !info.EstimateKnown || (entry != "count" && info.EstimatedTokens < preparedBytes.Load()+100) {
				t.Fatalf("prepared real entry estimate incomplete: info=%+v prepared-bytes=%d", info, preparedBytes.Load())
			}
			_, _, finished, result := gate.permits[0].snapshot()
			wantTokens := int64(17)
			if entry == "count" || invalidCompression {
				wantTokens = 0
			}
			if finished != 1 || result.Complete != !invalidCompression || result.SchedulerTokens != wantTokens {
				t.Fatalf("entry lifecycle: %d %+v", finished, result)
			}
		})
	}
}

type claudeAttemptErrorBody struct {
	io.Reader
	closeErr error
}

func (b *claudeAttemptErrorBody) Close() error { return b.closeErr }

func TestClaudeHTTPAttemptSettlementFailurePreservesResponse(t *testing.T) {
	settlementErr := errors.New("synthetic settlement persistence failure")
	for _, kind := range []string{"success", "close-error", "transport-error"} {
		t.Run(kind, func(t *testing.T) {
			gate := &claudeAttemptTestGate{finishErr: settlementErr}
			ctx := cliproxyexecutor.WithHTTPAttemptGate(context.Background(), gate)
			transportErr := errors.New("synthetic terminal transport failure")
			closeErr := errors.New("synthetic upstream close failure")
			client := &http.Client{Transport: claudeAttemptRoundTripFunc(func(*http.Request) (*http.Response, error) {
				if kind == "transport-error" {
					return nil, transportErr
				}
				var body io.ReadCloser = io.NopCloser(strings.NewReader(claudeAttemptResponseJSON))
				if kind == "close-error" {
					body = &claudeAttemptErrorBody{Reader: strings.NewReader(claudeAttemptResponseJSON), closeErr: closeErr}
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
			})}
			resp, err := doClaudeHTTPWithTransportRetry(ctx, client, claudeAttemptRequest(t, ctx, "http://localhost/v1/messages"))
			if kind == "transport-error" {
				if !errors.Is(err, transportErr) || errors.Is(err, settlementErr) || cliproxyexecutor.IsHTTPAttemptGateError(err) {
					t.Fatalf("settlement replaced transport error: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				attempt := helps.ClaimClaudeAttemptResponse(resp)
				body := attempt.ObserveDecoded(resp.Body)
				data, readErr := io.ReadAll(body)
				if readErr != nil || string(data) != claudeAttemptResponseJSON {
					t.Fatalf("settlement rewrote success/EOF: %q %v", data, readErr)
				}
				errClose := body.Close()
				if kind == "close-error" {
					if errClose != closeErr {
						t.Fatalf("original close error changed: %v", errClose)
					}
				} else if errClose != nil {
					t.Fatal(errClose)
				}
				if !errors.Is(attempt.SettlementError(), settlementErr) {
					t.Fatal("settlement diagnostic missing")
				}
			}
			_, _, finished, result := gate.permits[0].snapshot()
			if finished != 1 || result.Complete != (kind != "transport-error") {
				t.Fatalf("bad settlement count/outcome: %d %+v", finished, result)
			}
		})
	}
}

type claudeAttemptUsageCapture struct {
	authID, barrier string
	mu              sync.Mutex
	records         []usage.Record
	done            chan struct{}
}

func (c *claudeAttemptUsageCapture) HandleUsage(_ context.Context, r usage.Record) {
	if r.AuthID != c.authID {
		return
	}
	if r.Model == c.barrier {
		close(c.done)
		return
	}
	c.mu.Lock()
	c.records = append(c.records, r)
	c.mu.Unlock()
}

type claudeAttemptNoopUsage struct{}

func (claudeAttemptNoopUsage) HandleUsage(context.Context, usage.Record) {}

func TestClaudeHTTPAttemptFailureReporting(t *testing.T) {
	usage.StartDefault(context.Background())
	for _, entry := range []string{"execute", "stream"} {
		for _, kind := range []string{"local-denial", "previous-failure", "ordinary-error"} {
			t.Run(entry+"/"+kind, func(t *testing.T) {
				authID := "synthetic-" + t.Name()
				capture := &claudeAttemptUsageCapture{authID: authID, barrier: "barrier-" + t.Name(), done: make(chan struct{})}
				pluginName := "pacing-failure-" + t.Name()
				usage.RegisterNamedPlugin(pluginName, capture)
				t.Cleanup(func() { usage.RegisterNamedPlugin(pluginName, claudeAttemptNoopUsage{}) })
				var sends atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					sends.Add(1)
					if kind == "previous-failure" {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, `{"error":{"type":"overloaded_error","message":"synthetic upstream failure"}}`)
				}))
				defer server.Close()
				blocked := errors.New("synthetic local admission refusal")
				gate := &claudeAttemptTestGate{before: func(n int) error {
					if kind == "local-denial" || n > 1 {
						return blocked
					}
					return nil
				}}
				ctx := context.Background()
				if kind != "ordinary-error" {
					ctx = cliproxyexecutor.WithHTTPAttemptGate(ctx, gate)
				}
				exec := NewClaudeExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{ID: authID, Provider: "claude", ProxyURL: "direct", Attributes: map[string]string{"api_key": "key-123", "base_url": server.URL}}
				req := cliproxyexecutor.Request{Model: "claude-3-5-sonnet-20241022", Payload: []byte(claudeAttemptRequestJSON)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}
				var err error
				if entry == "execute" {
					_, err = exec.Execute(ctx, auth, req, opts)
				} else {
					_, err = exec.ExecuteStream(ctx, auth, req, opts)
				}
				if err == nil {
					t.Fatal("fixture unexpectedly succeeded")
				}
				usage.PublishRecord(ctx, usage.Record{AuthID: authID, Provider: "claude", Model: capture.barrier})
				select {
				case <-capture.done:
				case <-time.After(3 * time.Second):
					t.Fatal("usage dispatcher barrier not reached")
				}
				capture.mu.Lock()
				records := append([]usage.Record(nil), capture.records...)
				capture.mu.Unlock()
				if kind == "local-denial" {
					if sends.Load() != 0 || len(records) != 0 {
						t.Fatalf("unsent denial published upstream usage: sends=%d records=%d", sends.Load(), len(records))
					}
				} else {
					if sends.Load() != 1 || len(records) != 1 || !records[0].Failed {
						t.Fatalf("actual failure was lost: sends=%d records=%+v", sends.Load(), records)
					}
					if kind == "previous-failure" {
						var gateErr *cliproxyexecutor.HTTPAttemptGateError
						if !errors.As(err, &gateErr) || gateErr.Previous == nil {
							t.Fatal("previous transport failure missing")
						}
						if records[0].Fail.Body != gateErr.Previous.Error() || strings.Contains(records[0].Fail.Body, blocked.Error()) {
							t.Fatalf("record used local refusal rather than actual failure: %q", records[0].Fail.Body)
						}
					} else if records[0].Fail.StatusCode != 503 {
						t.Fatalf("ordinary failure status changed: %+v", records[0].Fail)
					}
				}
			})
		}
	}
}
