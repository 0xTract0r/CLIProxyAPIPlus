package executor

import "github.com/MoeForge/moego/ojson"

func normalizeOrdinaryClaudeCache(doc *ojson.Doc, explicitTTL bool) {
	enforceCacheControlLimitDoc(doc, 4)
	if !explicitTTL {
		normalizeCacheControlTTLDoc(doc)
	}
}
