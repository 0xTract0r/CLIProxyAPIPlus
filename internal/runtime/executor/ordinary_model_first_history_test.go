package executor

import (
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func issue4959ResponsesModelFirstPayload() []byte {
	return []byte(`{"model":"gemini-3.7-flash-high","input":[
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
	]}`)
}

func assertIssue4959LeadingUserContents(t *testing.T, contents []ojson.Result) {
	t.Helper()
	if len(contents) < 2 {
		t.Fatalf("contents too short: %d", len(contents))
	}
	leadingText := contents[0].Get("parts.0.text")
	if contents[0].Get("role").String() != "user" || !leadingText.Exists() || leadingText.String() != "" {
		t.Fatal("synthetic leading user was missing")
	}
	var text []string
	for _, content := range contents[1:] {
		if content.Get("role").String() != "model" {
			continue
		}
		for _, part := range content.Get("parts").Array() {
			if value := part.Get("text").String(); value != "" {
				text = append(text, value)
			}
		}
	}
	if len(text) != 2 || text[0] != "first" || text[1] != "second" {
		t.Fatalf("model history was lost or reordered: %v", text)
	}
}
