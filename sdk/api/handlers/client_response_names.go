package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const clientResponseNamesKey = "local-client-response-names"

type clientResponseNames struct {
	alias  string
	target string
}

// SetClientResponseNames opts a local route into using public names on its
// client response. The execution request and usage metadata retain the target.
func SetClientResponseNames(c *gin.Context, alias, target string) {
	c.Set(clientResponseNamesKey, clientResponseNames{alias: alias, target: target})
}

func ClientResponseNamesEnabled(c *gin.Context) bool {
	if c == nil {
		return false
	}
	_, exists := c.Get(clientResponseNamesKey)
	return exists
}

// ApplyClientModelLabel uses an explicit local alias when a successful
// response identifies the configured target. Keep other reported models intact.
func ApplyClientModelLabel(c *gin.Context, body []byte) []byte {
	if c == nil {
		return body
	}
	value, _ := c.Get(clientResponseNamesKey)
	names, ok := value.(clientResponseNames)
	if !ok || names.alias == names.target || !gjson.ValidBytes(body) {
		return body
	}
	if gjson.GetBytes(body, "error").Exists() {
		return body
	}
	reported := gjson.GetBytes(body, "model")
	if reported.Type != gjson.String {
		return body
	}
	target := thinking.ParseSuffix(names.target)
	if reported.Str != names.target && (!target.HasSuffix || reported.Str != target.ModelName) {
		return body
	}
	if changed, err := sjson.SetBytes(body, "model", names.alias); err == nil {
		return changed
	}
	return body
}
