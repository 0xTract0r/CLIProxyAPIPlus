package executor

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"testing"
)

// SD15/F显式always优先；confirmed只是auto的UA前提，不关闭显式always。
func TestClaudeMain396ForkModeAuthority(t *testing.T) {
	for _, mode := range []string{"auto", "always", "never"} {
		for _, native := range []bool{false, true} {
			label := "non-native"
			ua := "ordinary-client/1.0"
			if native {
				label = "native"
				ua = "claude-cli/2.1.280 (external, cli)"
			}
			t.Run(mode+"/"+label, func(t *testing.T) {
				auth := &coreauth.Auth{Metadata: map[string]any{"cloak_mode": mode}}
				policy, _ := resolveClaudeWirePolicy(&config.Config{}, auth, "sk-ant-oat-synthetic396", native, ua)
				want := mode == "always" || mode == "auto" && !native
				if policy.Cloak != want || !policy.OAuth || policy.ConfirmedClaudeCode != native {
					t.Fatalf("mode=%s native=%v policy=%+v wantCloak=%v", mode, native, policy, want)
				}
			})
		}
	}
}
