package executor

import (
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	authpkg "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// HTTP parser之后仍要求唯一逻辑字段及唯一值；重复大小写/重复值均不合并。
func uniqueWireHeader235(h http.Header, name string) (string, error) {
	keys := 0
	var values []string
	for key, v := range h {
		if strings.EqualFold(key, name) {
			keys++
			values = v
		}
	}
	if keys != 1 || len(values) != 1 || values[0] == "" {
		return "", fmt.Errorf("ambiguous or missing %s: keys=%d values=%d", name, keys, len(values))
	}
	return values[0], nil
}
func TestWire235ConflictingHeaderSpellingsRejected(t *testing.T) {
	for _, h := range []http.Header{{"Anthropic-Beta": {"a"}, "anthropic-beta": {"b"}}, {"Anthropic-Beta": {"a", "b"}}, {"Anthropic-Beta": {"same"}, "anthropic-beta": {"same"}}} {
		if _, err := uniqueWireHeader235(h, "Anthropic-Beta"); err == nil {
			t.Fatal("duplicate logical header accepted")
		}
	}
	if value, err := uniqueWireHeader235(http.Header{"Anthropic-Beta": {"one"}}, "Anthropic-Beta"); err != nil || value != "one" {
		t.Fatal("single real HTTP field rejected")
	}
}
func TestWire235OAuthBrowserAccessDefaultsOff(t *testing.T) {
	disabled := false
	for _, cfg := range []*config.Config{nil, {}, {ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{ReplayWireHeaderOrder: &disabled}}} {
		req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
		auth := &authpkg.Auth{Metadata: map[string]any{"access_token": "sk-ant-oat01-wire"}}
		if err := applyClaudeHeaders(req, auth, "sk-ant-oat01-wire", false, nil, []byte(`{"model":"claude-opus-5"}`), cfg, nil, false); err != nil {
			t.Fatal(err)
		}
		for key := range req.Header {
			if strings.EqualFold(key, "Anthropic-Dangerous-Direct-Browser-Access") {
				t.Fatal("default OAuth enabled browser-access")
			}
		}
	}
}
