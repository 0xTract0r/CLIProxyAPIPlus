package signature

import (
	"bytes"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestOrdinaryOutboundGeminiSignatureValueUnchanged(t *testing.T) {
	const issued = "native-opaque-test-value=="
	raw := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"weather","args":{}},"thoughtSignature":"native-opaque-test-value=="},{"functionCall":{"name":"forecast","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"weather","response":{"thoughtSignature":42}}}]}]}`)
	if got := GeminiReplaySignatureOrBypass(issued, SignatureBlockKindGeminiFunctionCall); got != issued {
		t.Fatal("translation changed opaque signature")
	}
	if got := GeminiReplaySignatureOrBypass("", SignatureBlockKindGeminiFunctionCall); got != "" {
		t.Fatal("translation created a signature")
	}
	for _, clean := range []func([]byte, string) []byte{SanitizeGeminiRequestThoughtSignatures, SanitizeNonVertexGeminiRequestThoughtSignatures} {
		if got := clean(raw, "contents"); !bytes.Equal(got, raw) {
			t.Fatal("request normalization changed a signature or unsigned part")
		}
	}
	doc := ojson.NewDoc(raw)
	if err := ValidateOutboundGeminiSignatures(doc); err != nil {
		t.Fatalf("opaque native wire field: %v", err)
	}
	if !bytes.Equal(doc.Bytes(), raw) {
		t.Fatal("validation changed request bytes")
	}
}

func TestOrdinaryOutboundGeminiInvalidSignatureFailsClosed(t *testing.T) {
	for name, field := range map[string]string{
		"placeholder":        "\"thoughtSignature\":\"" + GeminiSkipThoughtSignatureValidator + "\"",
		"empty":              `"thoughtSignature":""`,
		"not a string":       `"thoughtSignature":42`,
		"duplicate":          `"thoughtSignature":"opaque-a","thoughtSignature":"opaque-b"`,
		"ambiguous spelling": `"thoughtSignature":"opaque-a","thought_signature":"opaque-b"`,
	} {
		t.Run(name, func(t *testing.T) {
			doc := ojson.NewDoc([]byte(`{"contents":[{"role":"model","parts":[{` + field + `}]}]}`))
			if err := ValidateOutboundGeminiSignatures(doc); err == nil {
				t.Fatal("accepted invalid signature field")
			}
		})
	}
}

func TestOrdinaryOutboundGeminiInteractionsRequiresOpaqueSignature(t *testing.T) {
	body := []byte(`{"input":[{"type":"thought","signature":"native-opaque-test-value=="},{"type":"function_result","result":{"signature":42}}]}`)
	doc := ojson.NewDoc(body)
	if err := ValidateOutboundGeminiInteractionsSignatures(doc); err != nil {
		t.Fatalf("opaque interactions field: %v", err)
	}
	if !bytes.Equal(doc.Bytes(), body) {
		t.Fatal("validation changed opaque provider value")
	}
	for name, raw := range map[string]string{
		"missing":      `{"input":[{"type":"thought"}]}`,
		"misplaced":    `{"input":[{"type":"thought","thoughtSignature":"opaque"}]}`,
		"invalid type": `{"input":[{"type":"thought","signature":7}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateOutboundGeminiInteractionsSignatures(ojson.NewDoc([]byte(raw))); err == nil {
				t.Fatal("accepted invalid interactions signature field")
			}
		})
	}
}
