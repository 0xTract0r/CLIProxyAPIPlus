package executor

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSystem416TaskPlacementProbe(t *testing.T) {
	for _, mode := range []string{"auto", "always", "never", "auto-native", "always-native", "uncloak"} {
		t.Run(mode, func(t *testing.T) {
			p := []byte(`{"model":"claude-opus-4-6","system":"Task constraint SYSTEM_ONLY_416: use staging database only; never write production records.","messages":[{"role":"user","content":"USER_TASK_416: inspect invoice totals without writing."}],"tools":[{"name":"audit_invoices","description":"read invoice totals","input_schema":{"type":"object"}}]}`)
			actualMode := strings.TrimSuffix(mode, "-native")
			ctx := context.Background()
			if strings.HasSuffix(mode, "-native") {
				g, _ := gin.CreateTestContext(httptest.NewRecorder())
				g.Request, _ = http.NewRequest("POST", "/v1/messages", nil)
				g.Request.Header.Set("User-Agent", "claude-cli/2.1.280 (external, cli)")
				ctx = context.WithValue(ctx, "gin", g)
			}
			a := &auth.Auth{ID: "system416", Attributes: map[string]string{"cloak_mode": actualMode}}
			out, _, err := applyCloaking(ctx, &config.Config{}, a, p, "sk-ant-oat-probe416", false, true)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) == 0 {
				t.Fatal("empty output")
			}
			user := strings.Contains(string(out), "USER_TASK_416")
			system := strings.Contains(string(out), "SYSTEM_ONLY_416")
			tool := gjson.GetBytes(out, "tools.0.name").String() == "audit_invoices"
			t.Logf("mode=%s system_only_task_retained=%v user_task_retained=%v tool_retained=%v neutral_present=%v", mode, system, user, tool, strings.Contains(string(out), "Use the available tools"))
			if !user || !tool {
				t.Fatal("ordinary user task or tool lost")
			}
			if (mode == "never" || mode == "auto-native" || mode == "uncloak") && !system {
				t.Fatal("noncloaking mode lost system task")
			}
			if (mode == "auto" || mode == "always" || mode == "always-native") && system {
				t.Fatal("F default OAuth sanitization unexpectedly changed")
			}
		})
	}
}
