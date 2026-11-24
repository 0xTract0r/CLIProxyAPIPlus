package executor

import (
	"testing"

	"github.com/MoeForge/moego/ojson"
	internalsignature "github.com/router-for-me/CLIProxyAPI/v8/internal/signature"
)

func TestGeminiVertexSanitizePreservesOpaqueServiceSignature(t *testing.T) {
	const input = `{"contents":[{"role":"user","parts":[{"text":"input"}]},{"role":"model","parts":[{"functionCall":{"name":"fixture","args":{}},"thoughtSignature":"fixture-opaque-from-service"}]}]}`
	out := internalsignature.SanitizeGeminiRequestThoughtSignatures([]byte(input), "contents")
	path := "contents.1.parts.0.thoughtSignature"
	if before, after := ojson.GetBytesNoCopy([]byte(input), path), ojson.GetBytesNoCopy(out, path); after.Raw != before.Raw {
		t.Fatal("sanitization modified a provider-supplied opaque field")
	}
}
