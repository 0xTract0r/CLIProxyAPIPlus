package signature

import "testing"

func TestOrdinaryGeminiIncompatibleDecisionHasNoReplacement(t *testing.T) {
	for _, value := range []struct {
		name  string
		input string
	}{
		{"missing", ""},
		{"incompatible", "not a provider signature"},
	} {
		t.Run(value.name, func(t *testing.T) {
			for _, kind := range []SignatureBlockKind{SignatureBlockKindGeminiFunctionCall, SignatureBlockKindGeminiModelPart, SignatureBlockKindUnknown, SignatureBlockKindClaudeThinking} {
				got := DecideSignatureCompatibility(SignatureProviderGemini, value.input, kind)
				want := SignatureActionDropSignature
				if kind == SignatureBlockKindClaudeThinking {
					want = SignatureActionDropBlock
				}
				if got.Compatible || got.Action != want || got.ReplacementSignature != "" || got.NormalizedSignature != "" {
					t.Errorf("block %s: expected %s and no signature content; compatible=%t, action=%s", kind, want, got.Compatible, got.Action)
				}
			}
		})
	}
}
