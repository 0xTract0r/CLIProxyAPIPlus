package executor

import (
	"github.com/MoeForge/moego/ojson"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"strings"
)

// codexFastEnabled reports the operator's opt-in for priority billing on a model.
func codexFastEnabled(auth *cliproxyauth.Auth, model string) bool {
	if auth == nil {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	raw := ""
	if len(auth.Attributes) > 0 {
		raw = strings.TrimSpace(auth.Attributes["fast_models"])
	}
	if raw == "" && len(auth.Metadata) > 0 {
		if value, ok := auth.Metadata["fast_models"]; ok {
			switch typed := value.(type) {
			case string:
				raw = strings.TrimSpace(typed)
			case []any:
				var names []string
				for _, item := range typed {
					if name, ok := item.(string); ok {
						names = append(names, name)
					}
				}
				raw = strings.Join(names, ",")
			case []string:
				raw = strings.Join(typed, ",")
			}
		}
	}
	if raw == "" {
		return false
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == "*" || entry == model {
			return true
		}
	}
	return false
}

// applyConfiguredCodexTier only supplies an operator-selected default. An explicit
// service_tier in the request, including an explicitly cheaper tier, takes precedence.
func applyConfiguredCodexTier(doc *ojson.Doc, auth *cliproxyauth.Auth, model string) {
	if doc == nil || doc.Get("service_tier").Exists() || !codexFastEnabled(auth, model) {
		return
	}
	_ = doc.Set("service_tier", "priority")
}
