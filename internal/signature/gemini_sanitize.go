package signature

import (
	"errors"
	"fmt"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// GeminiReplaySignatureOrBypass is the legacy translation entry point.
// A supplied opaque signature is passed through exactly as received.
func GeminiReplaySignatureOrBypass(rawSignature string, _ SignatureBlockKind) string {
	if !isAllowedOutboundGeminiSignature(rawSignature) {
		return ""
	}
	return rawSignature
}

func SanitizeGeminiRequestThoughtSignatures(payload []byte, contentsPath string) []byte {
	return SanitizeGeminiSystemInstruction(payload, contentsPath)
}

func SanitizeGeminiRequestThoughtSignaturesDoc(doc *ojson.Doc, contentsPath string) {
	SanitizeGeminiSystemInstructionDoc(doc, contentsPath)
}

func SanitizeNonVertexGeminiRequestThoughtSignatures(payload []byte, contentsPath string) []byte {
	return SanitizeGeminiSystemInstruction(payload, contentsPath)
}

func SanitizeNonVertexGeminiRequestThoughtSignaturesDoc(doc *ojson.Doc, contentsPath string) {
	SanitizeGeminiSystemInstructionDoc(doc, contentsPath)
}

var errInvalidGeminiThoughtSignature = errors.New("invalid Gemini thought signature")

func isAllowedOutboundGeminiSignature(value string) bool {
	return strings.TrimSpace(value) != "" &&
		!IsGeminiThoughtSignatureBypass(SignaturePayloadWithoutProviderPrefix(value))
}

// ValidateOutboundGeminiSignatures checks the final GenerateContent request.
// It rejects recognized foreign formats and leaves opaque values to Gemini.
func ValidateOutboundGeminiSignatures(doc *ojson.Doc) error {
	contents := doc.Get("contents")
	for _, content := range contents.Array() {
		for _, part := range content.Get("parts").Array() {
			if err := validateGeminiSignatureFields(part, "thoughtSignature", "thought_signature"); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateOutboundGeminiInteractionsSignatures checks the final Interactions
// request. A thought input requires one signature in its protocol field.
func ValidateOutboundGeminiInteractionsSignatures(doc *ojson.Doc) error {
	for _, item := range doc.Get("input").Array() {
		if item.Get("type").String() != "thought" {
			continue
		}
		if err := validateGeminiSignatureFields(item, "signature", "thoughtSignature", "thought_signature"); err != nil {
			return err
		}
		if !item.Get("signature").Exists() {
			return errInvalidGeminiThoughtSignature
		}
	}
	return nil
}

func validateGeminiSignatureFields(part ojson.Result, names ...string) error {
	found := false
	valid := true
	part.ForEach(func(key, value ojson.Result) bool {
		for _, name := range names {
			if key.String() != name {
				continue
			}
			valid = !found && value.Type == ojson.String && isAllowedOutboundGeminiSignature(value.String()) &&
				!isRecognizedForeignGeminiSignature(value.String())
			found = true
			return valid
		}
		return true
	})
	if !valid {
		return errInvalidGeminiThoughtSignature
	}
	return nil
}

// Reject only classifiers backed by recognized foreign wire structures.
func isRecognizedForeignGeminiSignature(value string) bool {
	switch DetectSignatureProvider(value) {
	case SignatureProviderClaude, SignatureProviderGPT, SignatureProviderMeta:
		return true
	default:
		return false
	}
}

// SanitizeGeminiSystemInstruction removes empty part objects ({}) from
// systemInstruction.parts (and system_instruction.parts) at the same envelope
// level as contentsPath.
func SanitizeGeminiSystemInstruction(payload []byte, contentsPath string) []byte {
	if len(payload) == 0 {
		return payload
	}
	doc := ojson.NewDoc(payload)
	if !sanitizeGeminiSystemInstructionDoc(doc, contentsPath) {
		return payload
	}
	return doc.Bytes()
}

// SanitizeGeminiSystemInstructionDoc removes empty part objects ({}) from
// systemInstruction.parts (and system_instruction.parts) on doc in place.
func SanitizeGeminiSystemInstructionDoc(doc *ojson.Doc, contentsPath string) {
	_ = sanitizeGeminiSystemInstructionDoc(doc, contentsPath)
}

func sanitizeGeminiSystemInstructionDoc(doc *ojson.Doc, contentsPath string) bool {
	if doc == nil {
		return false
	}
	prefix := ""
	if trimmed := strings.TrimSpace(contentsPath); trimmed != "" {
		if idx := strings.LastIndex(trimmed, "."); idx > 0 {
			prefix = trimmed[:idx+1]
		}
	}
	modified := false
	for _, key := range []string{"systemInstruction", "system_instruction"} {
		sysPath := prefix + key
		sys := doc.Get(sysPath)
		if !sys.Exists() || !sys.IsObject() {
			continue
		}
		parts := sys.Get("parts")
		if !parts.IsArray() {
			continue
		}
		var emptyIndices []int
		totalParts := 0
		parts.ForEach(func(_, part ojson.Result) bool {
			if isEmptyGeminiPartObject(part) {
				emptyIndices = append(emptyIndices, totalParts)
			}
			totalParts++
			return true
		})
		if len(emptyIndices) == 0 {
			continue
		}
		if len(emptyIndices) == totalParts {
			if doc.Delete(sysPath) == nil {
				modified = true
			}
			continue
		}
		for i := len(emptyIndices) - 1; i >= 0; i-- {
			if doc.Delete(fmt.Sprintf("%s.parts.%d", sysPath, emptyIndices[i])) == nil {
				modified = true
			}
		}
	}
	return modified
}

func isEmptyGeminiPartObject(part ojson.Result) bool {
	if !part.IsObject() {
		return false
	}
	hasField := false
	part.ForEach(func(_, _ ojson.Result) bool {
		hasField = true
		return false
	})
	return !hasField
}

var geminiPartThoughtSignaturePaths = []string{
	"thoughtSignature",
	"thought_signature",
	"functionCall.thoughtSignature",
	"functionCall.thought_signature",
	"functionResponse.thoughtSignature",
	"functionResponse.thought_signature",
	"extra_content.google.thought_signature",
}

func geminiPartThoughtSignature(part ojson.Result) (string, bool) {
	for _, path := range geminiPartThoughtSignaturePaths {
		result := part.Get(path)
		if result.Exists() {
			return result.String(), true
		}
	}
	return "", false
}

func hasNormalizedGeminiPartThoughtSignature(part ojson.Result, replaySignature string) bool {
	canonicalCount := 0
	part.ForEach(func(key, _ ojson.Result) bool {
		if key.String() == "thoughtSignature" {
			canonicalCount++
		}
		return true
	})
	canonical := part.Get("thoughtSignature")
	if canonicalCount != 1 || canonical.Type != ojson.String || canonical.String() != replaySignature {
		return false
	}
	for _, path := range geminiPartThoughtSignaturePaths[1:] {
		if part.Get(path).Exists() {
			return false
		}
	}
	return true
}
