package helps

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestOrdinaryUsageStreamTTFTWaitsForContent(t *testing.T) {
	r := NewUsageReporter(context.Background(), "openai", "example", nil)
	r.SetStream(true)
	r.ttftStart = time.Now().Add(-time.Second)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("data: {}\n\n"))}
	r.ObserveResponse(resp)
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	fallback := r.ttftDuration()
	if fallback <= 0 || r.IsTTFTSet() {
		t.Fatal("metadata committed an effective TTFT")
	}
	// Change only the test clock origin, so this verifies ordering without a sleep.
	r.ttftStart = time.Now().Add(-2 * time.Second)
	r.ObserveContentEvent([]byte(`{"type":"response.output_text.delta","delta":"fixture"}`))
	content := r.ttftDuration()
	r.ObserveTokenEvent(true)
	if !r.IsTTFTSet() || content <= fallback || r.ttftDuration() != content {
		t.Fatal("first content must win exactly once over packet fallback")
	}
}

func TestOrdinaryUsageStructuredSnapshotMatchesStream(t *testing.T) {
	const payload = `{"usage":{"input_tokens":19,"output_tokens":9,"total_tokens":28,"input_tokens_details":{"cached_tokens":7,"cache_write_tokens":2},"output_tokens_details":{"reasoning_tokens":3}}}`
	unary := ParseOpenAIUsage([]byte(payload))
	var stream StreamUsageBuffer
	stream.ObserveOpenAIStream([]byte("data: " + payload))
	streamed, ok := stream.Detail()
	for _, v := range []struct{ input, output, cached, read, write, reasoning, total int64 }{
		{unary.InputTokens, unary.OutputTokens, unary.CachedTokens, unary.CacheReadTokens, unary.CacheCreationTokens, unary.ReasoningTokens, unary.TotalTokens},
		{streamed.InputTokens, streamed.OutputTokens, streamed.CachedTokens, streamed.CacheReadTokens, streamed.CacheCreationTokens, streamed.ReasoningTokens, streamed.TotalTokens},
	} {
		if !ok || v.input != 19 || v.output != 9 || v.cached != 7 || v.read != 7 || v.write != 2 || v.reasoning != 3 || v.total != 28 {
			t.Fatalf("usage mismatch: %+v", v)
		}
	}
}
