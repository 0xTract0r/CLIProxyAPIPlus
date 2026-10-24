package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func fixture432(t *testing.T, keys []config.ClaudeKey) (*Handler, *httptest.Server, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte("claude-api-key: []\n"), 0600)
	cfg := &config.Config{ClaudeKey: keys}
	if err := config.SaveConfigPreserveComments(p, cfg); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: p}
	r := gin.New()
	r.PUT("/v0/management/claude-api-key", h.PutClaudeKeys)
	r.GET("/v0/management/claude-api-key", h.GetClaudeKeys)
	s := httptest.NewServer(r)
	t.Cleanup(s.Close)
	return h, s, p
}
func request432(t *testing.T, s *httptest.Server, method string, body any) (int, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, s.URL+"/v0/management/claude-api-key", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}
func flag432(k config.ClaudeKey) *bool {
	if k.Cloak == nil {
		return nil
	}
	return k.Cloak.PreserveCallerSystem
}
func inspect432(t *testing.T, s *httptest.Server, p string) [][]config.ClaudeKey {
	t.Helper()
	code, raw := request432(t, s, "GET", nil)
	if code != 200 {
		t.Fatal(code)
	}
	var got struct {
		Keys []config.ClaudeKey `json:"claude-api-key"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfigOptional(p, false)
	if err != nil {
		t.Fatal(err)
	}
	return [][]config.ClaudeKey{got.Keys, cfg.ClaudeKey}
}
func TestClaudePreserve432PutPresence(t *testing.T) {
	for _, wrapper := range []bool{false, true} {
		for _, old := range []bool{false, true} {
			for _, shape := range []string{"absent", "empty", "present", "false", "true", "case-false", "null", "parent-null"} {
				for _, reverse := range []bool{false, true} {
					t.Run(fmt.Sprintf("wrapper%v/old%v/%s/reverse%v", wrapper, old, shape, reverse), func(t *testing.T) {
						oldA, oldB := old, !old
						h, s, p := fixture432(t, []config.ClaudeKey{{APIKey: "same", BaseURL: "https://a.invalid", Cloak: &config.CloakConfig{Mode: "always", PreserveCallerSystem: &oldA}}, {APIKey: "same", BaseURL: "https://b.invalid", Cloak: &config.CloakConfig{Mode: "never", PreserveCallerSystem: &oldB}}})
						a := map[string]any{"api-key": "same", "base-url": "https://a.invalid"}
						b := map[string]any{"api-key": "same", "base-url": "https://b.invalid"}
						want := &old
						switch shape {
						case "empty":
							a["cloak"] = map[string]any{}
						case "present":
							a["cloak"] = map[string]any{"sensitive-words": []string{"SECRET"}}
						case "false", "case-false":
							f := false
							want = &f
							a["cloak"] = map[string]any{"preserve-caller-system": false}
							if shape == "case-false" {
								a["cloak"] = map[string]any{"Preserve-Caller-System": false}
							}
						case "true":
							v := true
							want = &v
							a["cloak"] = map[string]any{"preserve-caller-system": true}
						case "null":
							want = nil
							a["cloak"] = map[string]any{"preserve-caller-system": nil}
						case "parent-null":
							want = nil
							a["cloak"] = nil
						}
						list := []map[string]any{a, b}
						if reverse {
							list[0], list[1] = list[1], list[0]
						}
						var body any = list
						if wrapper {
							body = map[string]any{"items": list}
						}
						code, raw := request432(t, s, "PUT", body)
						if code != 200 {
							t.Fatalf("PUT %d %s", code, raw)
						}
						for _, keys := range inspect432(t, s, p) {
							if len(keys) != 2 {
								t.Fatal("list lost")
							}
							for _, k := range keys {
								w := want
								if k.BaseURL == "https://b.invalid" {
									w = &oldB
								}
								v := flag432(k)
								if (v == nil) != (w == nil) || (v != nil && *v != *w) {
									t.Fatalf("authority changed for %s: %v want %v", k.BaseURL, v, w)
								}
								if shape == "present" && k.BaseURL == "https://a.invalid" && (len(k.Cloak.SensitiveWords) != 1 || k.Cloak.SensitiveWords[0] != "SECRET") {
									t.Fatal("sibling lost")
								}
							}
						}
						for _, k := range h.cfg.ClaudeKey {
							if flag432(k) == &oldA || flag432(k) == &oldB {
								t.Fatal("pointer alias")
							}
						}
					})
				}
			}
		}
	}
}
func TestClaudePreserve432Identity(t *testing.T) {
	for _, mode := range []string{"routing-edit", "route-disambiguation", "ambiguous-old", "ambiguous-route", "duplicate-new", "new-key", "explicit-clear-ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			v := false
			keys := []config.ClaudeKey{{APIKey: "key", BaseURL: "https://a.invalid", Prefix: "one", ProxyURL: "direct", Headers: map[string]string{"X-Test": "one"}, Cloak: &config.CloakConfig{PreserveCallerSystem: &v}}}
			item := map[string]any{"api-key": "key", "base-url": "https://a.invalid", "prefix": "one", "proxy-url": "direct", "headers": map[string]string{"X-Test": "one"}}
			codeWant := 200
			nilWant := false
			switch mode {
			case "routing-edit":
				item["prefix"] = "changed"
				item["proxy-url"] = "http://proxy.invalid:8888"
			case "route-disambiguation":
				k := keys[0]
				k.Prefix = "two"
				x := true
				k.Cloak = &config.CloakConfig{PreserveCallerSystem: &x}
				keys = append(keys, k)
			case "ambiguous-old", "explicit-clear-ambiguous":
				keys = append(keys, keys[0])
				codeWant = 400
				if mode == "explicit-clear-ambiguous" {
					item["cloak"] = map[string]any{"preserve-caller-system": nil}
					codeWant = 200
					nilWant = true
				}
			case "ambiguous-route":
				k := keys[0]
				k.Headers = map[string]string{"X-Test": "two"}
				keys = append(keys, k)
				delete(item, "headers")
				codeWant = 400
			case "duplicate-new":
				codeWant = 400
			case "new-key":
				item["api-key"] = "new"
				nilWant = true
			}
			h, s, p := fixture432(t, keys)
			before, _ := os.ReadFile(p)
			items := []map[string]any{item}
			if mode == "duplicate-new" {
				items = append(items, item)
			}
			code, body := request432(t, s, "PUT", items)
			if code != codeWant {
				t.Fatalf("PUT %d want %d %s", code, codeWant, body)
			}
			if code == 400 {
				after, _ := os.ReadFile(p)
				if !bytes.Equal(before, after) || len(h.cfg.ClaudeKey) != len(keys) {
					t.Fatal("reject mutated")
				}
				return
			}
			for _, list := range inspect432(t, s, p) {
				v := flag432(list[0])
				if (v == nil) != nilWant || (v != nil && *v) {
					t.Fatal("identity flag lost")
				}
			}
		})
	}
}
