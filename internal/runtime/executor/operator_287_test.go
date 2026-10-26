package executor
import (
 "net/http"
 "testing"
 cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)
// This oracle is the fixed fork's Messages floor, not captured candidate output.
func TestOperator287ForkFloorAndOverrides(t *testing.T) {
 const floor = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07"
 const want = "claude-code-20250219,oauth-2025-04-20,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07,operator-a-2099-01-01,operator-b-2099-01-01"
 cfg := parseIssue5093Config(t, `upstream: {claude: {header-defaults: {anthropic-beta-strip: ["interleaved-thinking-2025-05-14", "fine-grained-tool-streaming-2025-05-14", "operator-a-2099-01-01"], anthropic-beta-append: ["operator-a-2099-01-01", "operator-b-2099-01-01", "operator-a-2099-01-01"]}}}`)
 if cfg.ClaudeHeaderDefaults.PinSoftwareBaseline { t.Fatal("probe must exercise unpinned fork floor") }
 incoming := http.Header{"Anthropic-Beta": []string{"fine-grained-tool-streaming-2025-05-14"}}
 auth := &cliproxyauth.Auth{ProxyURL:"direct", Attributes: map[string]string{"api_key":"key-operator287"}}
 baseline:=newClaudeHeaderTestRequest(t,nil)
 if err:=applyClaudeHeaders(baseline,auth,"key-operator287",false,nil,nil,nil,nil,false);err!=nil { t.Fatal(err) }
 if got:=baseline.Header.Get("Anthropic-Beta");got!=floor { t.Fatalf("F baseline=%q want=%q",got,floor) }
 req:=newClaudeHeaderTestRequest(t,incoming)
 for i:=0;i<2;i++ {
  if err:=applyClaudeHeaders(req,auth,"key-operator287",false,nil,[]byte(`{"model":"claude-opus-5"}`),cfg,incoming,false);err!=nil { t.Fatal(err) }
  if got:=req.Header.Get("Anthropic-Beta");got!=want { t.Fatalf("application %d=%q want=%q",i,got,want) }
 }
}
