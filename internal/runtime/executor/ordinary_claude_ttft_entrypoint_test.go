package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type ordinaryTTFTUsageCapture struct {
	authID  string
	records chan usage.Record
}

func (c ordinaryTTFTUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if c.records != nil && record.AuthID == c.authID {
		c.records <- record
	}
}

type ordinaryTTFTReadStage struct {
	after   time.Duration
	content string
}
type ordinaryTTFTBody struct{ stages []ordinaryTTFTReadStage }

func (b *ordinaryTTFTBody) Read(p []byte) (int, error) {
	if len(b.stages) == 0 {
		return 0, io.EOF
	}
	stage := &b.stages[0]
	// Each read releases its fragment at a precisely controlled virtual time.
	time.Sleep(stage.after)
	stage.after = 0
	n := copy(p, stage.content)
	stage.content = stage.content[n:]
	if stage.content == "" {
		b.stages = b.stages[1:]
	}
	return n, nil
}
func (*ordinaryTTFTBody) Close() error { return nil }

func TestOrdinaryClaudeExecutePublishesUpstreamTTFT(t *testing.T) {
	const model = "claude-opus-5-5"
	usage.DefaultManager().Start(context.Background())
	for _, tc := range []struct {
		name, contentType string
		stages            []ordinaryTTFTReadStage
		ttft, latency     time.Duration
	}{
		{"buffered SSE", "  TeXt/EvEnT-StReAm ; charset=utf-8  ", []ordinaryTTFTReadStage{
			{time.Second, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5-5\",\"content\":[],\"usage\":{\"input_tokens\":5}}}\n\n"},
			{2 * time.Second, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"hel\"}}\n\n"},
			{time.Second, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n"},
			{5 * time.Second, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\ndata: {\"type\":\"message_stop\"}\n\n"},
		}, 3 * time.Second, 9 * time.Second},
		{"zero content", "text/event-stream", []ordinaryTTFTReadStage{
			{time.Second, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5-5\",\"content\":[],\"usage\":{\"input_tokens\":5}}}\n\n"},
			{5 * time.Second, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\ndata: {\"type\":\"message_stop\"}\n\n"},
		}, time.Second, 6 * time.Second},

		{"native JSON fallback", "application/json", []ordinaryTTFTReadStage{
			{2 * time.Second, `{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"hello"}],`},
			{5 * time.Second, `"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`},
		}, 2 * time.Second, 7 * time.Second},
		{"near MIME uses JSON reader", "text/event-streamx", []ordinaryTTFTReadStage{
			{2 * time.Second, `{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"hello"}],`},
			{5 * time.Second, `"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`},
		}, 2 * time.Second, 7 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := make(chan usage.Record, 4)
			name := "ordinary-claude-execute-ttft"
			usage.RegisterNamedPlugin(name, ordinaryTTFTUsageCapture{authID: t.Name(), records: records})
			t.Cleanup(func() { usage.RegisterNamedPlugin(name, ordinaryTTFTUsageCapture{}) })
			synctest.Test(t, func(t *testing.T) {
				transport := canonicalClaudeTestTransport(func(req *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					if req.Method != http.MethodPost || req.URL.Path != "/v1/messages" || !ojson.GetBytes(body, "stream").Bool() {
						t.Fatal("fixture did not exercise a normal cross-format request requesting SSE")
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {tc.contentType}}, Body: &ordinaryTTFTBody{stages: append([]ordinaryTTFTReadStage(nil), tc.stages...)}, Request: req}, nil
				})
				ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "claude", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "https://claude-fixture.invalid"}}
				input := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`)
				res, err := NewClaudeExecutor(&config.Config{}).Execute(ctx, auth, cliproxyexecutor.Request{Model: model, Payload: input}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI, OriginalRequest: input})
				wantText := map[string]string{"buffered SSE": "lo", "native JSON fallback": "hello", "near MIME uses JSON reader": "hello"}[tc.name]
				if err != nil || (wantText != "" && !strings.Contains(string(res.Payload), wantText)) {
					t.Fatalf("ordinary response missing: %v", err)
				}
			})
			select {
			case record := <-records:
				if record.Failed || record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 3 || record.TTFT != tc.ttft || record.Latency != tc.latency {
					t.Fatalf("published TTFT=%v latency=%v failed=%t tokens=%d/%d; want %v/%v successful 5/3", record.TTFT, record.Latency, record.Failed, record.Detail.InputTokens, record.Detail.OutputTokens, tc.ttft, tc.latency)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("usage record missing")
			}
		})
	}
}
