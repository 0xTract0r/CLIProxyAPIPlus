package auth

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
)

const warmupTaskMaxBytes = 256 << 10

// Normalize only text wrapping and whitespace, without keyword-based trimming.
// Complete known auxiliary notices are not task text. No plaintext is retained
// in the summary; oversize or opaque first tasks remain conservative.
func warmupCanonicalTask(raw any) string {
	if !warmupFreshUserMessage(raw) {
		return ""
	}
	message := raw.(map[string]any)
	texts := warmupTextBlocks(message["content"])
	parts := make([]string, 0, len(texts))
	size := 0
	for _, text := range texts {
		size += len(text)
		if size > warmupTaskMaxBytes {
			return ""
		}
		if _, auxiliary := warmupNoticeUnits(text); auxiliary {
			continue
		}
		if normalized := strings.Join(strings.Fields(text), " "); normalized != "" {
			parts = append(parts, normalized)
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// Compare the whole parent task, not a fixed short prefix: real independent
// children can share large leading context blocks with their parent. This
// catches exact normalized reuse/extensions, not paraphrases or arbitrary
// prepended prose. Temporary parsing is limited to the first unbound decision.
func warmupInheritedTaskPrefix(payload []byte, parent warmupRequestSummary) bool {
	if parent.taskBytes <= 0 || len(payload) == 0 || len(payload) > 4*1024*1024 {
		return true
	}
	var request struct {
		Messages []any `json:"messages"`
	}
	if json.Unmarshal(payload, &request) != nil || len(request.Messages) == 0 {
		return true
	}
	task := warmupCanonicalTask(request.Messages[0])
	if task == "" {
		return true
	}
	return len(task) >= parent.taskBytes && sha256.Sum256([]byte(task[:parent.taskBytes])) == parent.task
}
