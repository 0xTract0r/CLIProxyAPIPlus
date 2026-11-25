package helps

import "strings"

// IsClaudeCodeClient recognizes the client name in an incoming user agent.
func IsClaudeCodeClient(userAgent string) bool {
	return strings.HasPrefix(userAgent, "claude-cli")
}
