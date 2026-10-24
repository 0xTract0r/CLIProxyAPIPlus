package executor

import (
	"bytes"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	trans "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"net/http"
	"strings"
	"testing"
)

func uniqueCountHeader243(h http.Header, name string) (string, error) {
	count := 0
	var values []string
	for key, v := range h {
		if strings.EqualFold(key, name) {
			count++
			values = v
		}
	}
	if count != 1 || len(values) != 1 || values[0] == "" {
		return "", fmt.Errorf("ambiguous or missing %s keys=%d values=%d", name, count, len(values))
	}
	return values[0], nil
}
func TestCount243HeaderConflictRejected(t *testing.T) {
	for _, h := range []http.Header{{"Anthropic-Beta": {"one"}, "anthropic-beta": {"two"}}, {"Anthropic-Beta": {"one", "two"}}, {"Anthropic-Beta": {"same"}, "anthropic-beta": {"same"}}} {
		if _, err := uniqueCountHeader243(h, "Anthropic-Beta"); err == nil {
			t.Fatal("duplicate logical header accepted")
		}
	}
}
func TestCount243TypedClaudeRetainsStrictFingerprint(t *testing.T) {
	if !codexRefreshSandboxChild(t) {
		return
	}
	payload := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"tools":[]}`)
	auth := &authpkg.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant-oat-native-count-shape"}}
	opts := ex.Options{SourceFormat: trans.FormatClaude, Headers: http.Header{"User-Agent": {"claude-cli/2.1.280 (external, cli)"}, "X-App": {"cli"}, "Anthropic-Beta": {"claude-code-20250219,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01"}}}
	got, _ := countReview216Official(t, payload, &config.Config{}, auth, opts)
	if !bytes.Equal(got, payload) {
		t.Fatalf("typed control body changed: got %s want %s", got, payload)
	}
	t.Log("AFTER_STRICT_TLS typed Claude control reached original body oracle")
}
