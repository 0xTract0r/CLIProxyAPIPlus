package signature

import (
	"bytes"
	"github.com/MoeForge/moego/ojson"
	"testing"
)

func TestSanitizeGeminiRequestThoughtSignaturesNativeParallelPreservesUnsignedSibling(t *testing.T) {
	nativeSignature := "fixture-provider-opaque=="
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}},"thoughtSignature":"` + nativeSignature + `"},{"functionCall":{"name":"second","args":{}}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != nativeSignature {
		t.Fatalf("first call signature = %q, want native signature; output=%s", got, out)
	}
	if signature := ojson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
		t.Fatalf("native unsigned sibling should remain unsigned; output=%s", out)
	}
	if &out[0] != &input[0] {
		t.Fatal("already-native parallel history was copied")
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesLeavesUnsignedThoughtUnsigned(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"text":"hidden","thought":true}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if signature := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature"); signature.Exists() {
		t.Fatalf("unsigned thought should remain unsigned; output=%s", out)
	}
	if &out[0] != &input[0] {
		t.Fatal("unsigned thought payload was copied")
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesReusesUnsignedFunctionResponsePayload(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"ok"}}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if &out[0] != &input[0] {
		t.Fatal("unsigned function response payload was copied")
	}
	if string(out) != string(input) {
		t.Fatalf("payload changed:\n got: %s\nwant: %s", out, input)
	}
}

func TestSanitizeGeminiRequestThoughtSignatures_SkipsToolCallAndToolResponseParts(t *testing.T) {
	// Server-side tool blocks (toolCall and toolResponse) carry their own signatures/envelopes
	// that must be echoed back to the API untouched.
	const arbitrarySig = "arbitrary_opaque_tool_sig_value"
	input := []byte(`{"contents":[{"role":"model","parts":[
		{"toolCall":{"toolType":"GOOGLE_SEARCH_WEB","args":{}},"thoughtSignature":"` + arbitrarySig + `"},
		{"toolResponse":{"toolType":"GOOGLE_SEARCH_WEB","response":{}},"thoughtSignature":"` + arbitrarySig + `"}
	]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := ojson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != arbitrarySig {
		t.Fatalf("toolCall thoughtSignature = %q, want preserved %q", got, arbitrarySig)
	}
	if got := ojson.GetBytes(out, "contents.0.parts.1.thoughtSignature").String(); got != arbitrarySig {
		t.Fatalf("toolResponse thoughtSignature = %q, want preserved %q", got, arbitrarySig)
	}
}

func TestSanitizeGeminiRequestThoughtSignatures_SkipsSnakeCaseToolCallAndResponseParts(t *testing.T) {
	// Defensively ensure snake_case tool_call and tool_response variants are also skipped
	const arbitrarySig = "arbitrary_snake_tool_sig"
	input := []byte(`{"contents":[{"role":"model","parts":[
		{"tool_call":{"tool_type":"GOOGLE_SEARCH_WEB","args":{}},"thought_signature":"` + arbitrarySig + `"},
		{"tool_response":{"tool_type":"GOOGLE_SEARCH_WEB","response":{}},"thought_signature":"` + arbitrarySig + `"}
	]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := ojson.GetBytes(out, "contents.0.parts.0.thought_signature").String(); got != arbitrarySig {
		t.Fatalf("tool_call thought_signature = %q, want preserved %q", got, arbitrarySig)
	}
	if got := ojson.GetBytes(out, "contents.0.parts.1.thought_signature").String(); got != arbitrarySig {
		t.Fatalf("tool_response thought_signature = %q, want preserved %q", got, arbitrarySig)
	}
}

func TestSanitizeGeminiRequestThoughtSignatures_StripsEmptySystemInstructionParts(t *testing.T) {
	t.Run("drops_systemInstruction_when_all_parts_are_empty_objects", func(t *testing.T) {
		input := []byte(`{"request":{"systemInstruction":{"parts":[{}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)
		out := SanitizeGeminiSystemInstruction(input, "request.contents")
		if ojson.GetBytes(out, "request.systemInstruction").Exists() {
			t.Fatalf("expected request.systemInstruction to be removed, got %s", out)
		}
	})

	t.Run("retains_non_empty_systemInstruction_parts", func(t *testing.T) {
		input := []byte(`{"systemInstruction":{"parts":[{},{"text":"keep rule"},{}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
		out := SanitizeGeminiSystemInstruction(input, "contents")
		parts := ojson.GetBytes(out, "systemInstruction.parts").Array()
		if len(parts) != 1 || parts[0].Get("text").String() != "keep rule" {
			t.Fatalf("unexpected systemInstruction.parts: %s", out)
		}
	})

	t.Run("reuses_payload_when_systemInstruction_parts_are_valid", func(t *testing.T) {
		input := []byte(`{"systemInstruction":{"parts":[{"text":"keep rule"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
		out := SanitizeGeminiSystemInstruction(input, "contents")
		if &out[0] != &input[0] {
			t.Fatal("valid systemInstruction payload was unexpectedly copied")
		}
	})
}

func TestOutboundGeminiDuplicateCanonicalSignatureRejectsWithoutMutation(t *testing.T) {
	input := []byte(`{"contents":[{"parts":[{"functionCall":{"name":"lookup"},"thoughtSignature":"opaque-fixture-a","thoughtSignature":"opaque-fixture-b"},{"text":"keep"}]}]}`)
	doc := ojson.NewDoc(input)
	if err := ValidateOutboundGeminiSignatures(doc); err == nil {
		t.Fatal("duplicate canonical field accepted")
	} else if bytes.Contains([]byte(err.Error()), []byte("opaque-fixture")) {
		t.Fatal("validation error exposed the signature")
	}
	if !bytes.Equal(doc.Bytes(), input) {
		t.Fatal("rejection changed the request or adjacent text")
	}
}

func TestOutboundGeminiKeepsOpaqueAndAdjacentContent(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"toolCall":{"id":"call1"},"thoughtSignature":"fixture-opaque-tool=="},{"functionCall":{"name":"lookup","args":{"key":"value"}},"thought_signature":"fixture-opaque-call=="},{"functionCall":{"name":"next","args":{}}},{"text":"keep"}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"value":"keep","thoughtSignature":42}}}]}]}`)
	doc := ojson.NewDoc(input)
	if err := ValidateOutboundGeminiSignatures(doc); err != nil {
		t.Fatalf("valid opaque field: %v", err)
	}
	if !bytes.Equal(doc.Bytes(), input) {
		t.Fatal("validation changed opaque fields, unsigned parts, or adjacent content")
	}
}

func TestOrdinaryGeminiWrappedSystemCleanupPreservesOrderedToolExchange(t *testing.T) {
	input := []byte(`{"request":{"system_instruction":{"parts":[{},{"text":"keep rule"},{}]},"contents":[{"role":"model","parts":[{"toolCall":{"id":"t1"},"thoughtSignature":"opaque-tool-envelope"},{"functionCall":{"name":"lookup"},"thoughtSignature":"opaque-function-envelope"}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"message":"keep result"}},"thoughtSignature":"opaque-result-envelope"},{"text":"keep trailing text"}]}]},"version":"fixture"}`)
	out := SanitizeGeminiSystemInstruction(input, "request.contents")
	want := ojson.GetBytes(input, "request.contents").Raw
	if got := ojson.GetBytes(out, "request.contents").Raw; got != want {
		t.Fatal("wrapped system cleanup changed ordered tool exchange bytes")
	}
	if got := ojson.GetBytes(out, "request.system_instruction.parts").Array(); len(got) != 1 || got[0].Get("text").String() != "keep rule" {
		t.Fatal("wrapped system cleanup did not retain the non-empty instruction")
	}
	if ojson.GetBytes(out, "version").String() != "fixture" {
		t.Fatal("wrapped system cleanup lost outer fields")
	}
	// The final GenerateContent validator consumes the unwrapped request. The
	// envelope values themselves remain opaque, including on function results.
	doc := ojson.NewDoc([]byte(ojson.GetBytes(out, "request").Raw))
	if err := ValidateOutboundGeminiSignatures(doc); err != nil {
		t.Fatalf("valid ordinary tool exchange: %v", err)
	}
	if got := doc.Get("contents").Raw; got != want {
		t.Fatal("validation changed ordered tool exchange bytes")
	}
}
