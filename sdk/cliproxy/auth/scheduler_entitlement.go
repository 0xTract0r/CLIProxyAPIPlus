package auth

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// schedulerModelEntitled narrows registered model access using the credential's
// known subscription. It never expands a registry model set.
func schedulerModelEntitled(auth *Auth, model string) bool {
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		switch {
		case registry.IsClaudeOpus1MAlias(model):
			return registry.ClaudePlanAllowsOpusLongContext(auth.SubscriptionPlanType(), false)
		case registry.IsClaudeOpusModelID(model):
			return registry.ClaudePlanAllowsOpus(auth.SubscriptionPlanType())
		}
	case "codex":
		if registry.IsCodexSparkModelID(model) {
			return registry.CodexPlanAllowsSpark(auth.SubscriptionPlanType())
		}
	}
	return true
}
