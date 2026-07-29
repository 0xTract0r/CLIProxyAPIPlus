package helps

// Fork-unique: these three helpers (hasGeminiFamilyUsageTokenFields,
// ParseGeminiCLIUsage, ParseGeminiCLIStreamUsage) exist only because upstream
// dropped the Gemini CLI usage variants during the split (see the doc comments
// directly above them in usage_helpers.go). They compile as ordinary
// unexported/exported functions with no call sites gated behind a build tag,
// so a future "dead code" cleanup could delete them silently without any test
// going red. This file pins their observable parsing/detection behavior for a
// representative set of Gemini CLI usage payloads.

import (
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

func TestHasGeminiFamilyUsageTokenFields(t *testing.T) {
	tests := map[string]struct {
		payload string
		want    bool
	}{
		"promptTokenCount only":        {`{"promptTokenCount":10}`, true},
		"candidatesTokenCount only":    {`{"candidatesTokenCount":5}`, true},
		"thoughtsTokenCount only":      {`{"thoughtsTokenCount":3}`, true},
		"totalTokenCount only":         {`{"totalTokenCount":20}`, true},
		"cachedContentTokenCount only": {`{"cachedContentTokenCount":4}`, true},
		"no gemini token fields":       {`{"foo":"bar"}`, false},
		"empty object":                 {`{}`, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			node := gjson.Parse(tc.payload)
			if got := hasGeminiFamilyUsageTokenFields(node); got != tc.want {
				t.Fatalf("hasGeminiFamilyUsageTokenFields(%s) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}

	// A zero-value Result (as returned when a lookup path doesn't exist)
	// must not be mistaken for a populated usageMetadata node.
	if got := hasGeminiFamilyUsageTokenFields(gjson.Result{}); got != false {
		t.Fatalf("hasGeminiFamilyUsageTokenFields(zero Result) = %v, want false", got)
	}
}

func TestParseGeminiCLIUsage(t *testing.T) {
	t.Run("response.usageMetadata wrapper", func(t *testing.T) {
		data := []byte(`{"response":{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15,"cachedContentTokenCount":3}}}`)
		detail := ParseGeminiCLIUsage(data)
		if detail.InputTokens != 10 || detail.OutputTokens != 5 || detail.TotalTokens != 15 {
			t.Fatalf("detail = %+v, want input=10 output=5 total=15", detail)
		}
		if detail.CachedTokens != 3 || detail.CacheReadTokens != 3 {
			t.Fatalf("detail = %+v, want cached=3 cacheRead=3", detail)
		}
	})

	t.Run("response.usage_metadata snake_case wrapper", func(t *testing.T) {
		data := []byte(`{"response":{"usage_metadata":{"promptTokenCount":8,"candidatesTokenCount":2,"totalTokenCount":10}}}`)
		detail := ParseGeminiCLIUsage(data)
		if detail.InputTokens != 8 || detail.OutputTokens != 2 || detail.TotalTokens != 10 {
			t.Fatalf("detail = %+v, want input=8 output=2 total=10", detail)
		}
	})

	t.Run("bare usageMetadata without response wrapper", func(t *testing.T) {
		data := []byte(`{"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":1,"totalTokenCount":5}}`)
		detail := ParseGeminiCLIUsage(data)
		if detail.InputTokens != 4 || detail.OutputTokens != 1 || detail.TotalTokens != 5 {
			t.Fatalf("detail = %+v, want input=4 output=1 total=5", detail)
		}
	})

	t.Run("bare usage_metadata without response wrapper", func(t *testing.T) {
		data := []byte(`{"usage_metadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`)
		detail := ParseGeminiCLIUsage(data)
		if detail.InputTokens != 2 || detail.OutputTokens != 1 || detail.TotalTokens != 3 {
			t.Fatalf("detail = %+v, want input=2 output=1 total=3", detail)
		}
	})

	t.Run("response wrapper takes precedence over bare usageMetadata", func(t *testing.T) {
		data := []byte(`{"response":{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":50,"totalTokenCount":150}},"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)
		detail := ParseGeminiCLIUsage(data)
		if detail.InputTokens != 100 || detail.OutputTokens != 50 || detail.TotalTokens != 150 {
			t.Fatalf("detail = %+v, want the response.usageMetadata values (input=100 output=50 total=150), got bare usageMetadata instead", detail)
		}
	})

	t.Run("no matching usage node returns zero Detail", func(t *testing.T) {
		data := []byte(`{"foo":"bar"}`)
		detail := ParseGeminiCLIUsage(data)
		if !reflect.DeepEqual(detail, usage.Detail{}) {
			t.Fatalf("detail = %+v, want zero-value usage.Detail{}", detail)
		}
	})
}

func TestParseGeminiCLIStreamUsage(t *testing.T) {
	t.Run("SSE data line with response.usageMetadata", func(t *testing.T) {
		line := []byte(`data: {"response":{"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}}`)
		detail, ok := ParseGeminiCLIStreamUsage(line)
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		if detail.InputTokens != 7 || detail.OutputTokens != 3 || detail.TotalTokens != 10 {
			t.Fatalf("detail = %+v, want input=7 output=3 total=10", detail)
		}
	})

	t.Run("raw JSON line with bare usage_metadata", func(t *testing.T) {
		line := []byte(`{"usage_metadata":{"promptTokenCount":6,"candidatesTokenCount":2,"totalTokenCount":8}}`)
		detail, ok := ParseGeminiCLIStreamUsage(line)
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		if detail.InputTokens != 6 || detail.OutputTokens != 2 || detail.TotalTokens != 8 {
			t.Fatalf("detail = %+v, want input=6 output=2 total=8", detail)
		}
	})

	t.Run("usageMetadata node present but empty yields false", func(t *testing.T) {
		line := []byte(`{"response":{"usageMetadata":{}}}`)
		detail, ok := ParseGeminiCLIStreamUsage(line)
		if ok {
			t.Fatalf("ok = true, want false for an empty usageMetadata object")
		}
		if !reflect.DeepEqual(detail, usage.Detail{}) {
			t.Fatalf("detail = %+v, want zero-value usage.Detail{}", detail)
		}
	})

	t.Run("no usageMetadata node at all yields false", func(t *testing.T) {
		_, ok := ParseGeminiCLIStreamUsage([]byte(`{"foo":"bar"}`))
		if ok {
			t.Fatalf("ok = true, want false when no usage node exists")
		}
	})

	t.Run("invalid JSON yields false", func(t *testing.T) {
		_, ok := ParseGeminiCLIStreamUsage([]byte(`not json`))
		if ok {
			t.Fatalf("ok = true, want false for invalid JSON")
		}
	})

	t.Run("SSE terminator sentinel yields false", func(t *testing.T) {
		_, ok := ParseGeminiCLIStreamUsage([]byte(`[DONE]`))
		if ok {
			t.Fatalf("ok = true, want false for the [DONE] SSE sentinel")
		}
	})

	t.Run("SSE event line yields false", func(t *testing.T) {
		_, ok := ParseGeminiCLIStreamUsage([]byte(`event: message`))
		if ok {
			t.Fatalf("ok = true, want false for an SSE event: line with no JSON payload")
		}
	})
}
