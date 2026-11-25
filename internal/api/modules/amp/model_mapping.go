// Package amp contains shared protocol helpers for active Amp routes.
package amp

import (
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
)

// MatchModel selects an explicit mapping before routing and preserves the
// caller's thinking suffix unless the operator already supplied a suffix.
// Routing and authorization of the chosen model are the caller's responsibility.
func MatchModel(requested string, mappings []config.AmpModelMapping) string {
	input := thinking.ParseSuffix(requested)
	apply := func(to string) string {
		if input.HasSuffix && input.RawSuffix != "" && !thinking.ParseSuffix(to).HasSuffix {
			return to + "(" + input.RawSuffix + ")"
		}
		return to
	}
	for _, name := range []string{requested, input.ModelName} {
		for _, mapping := range mappings {
			if !mapping.Regex && strings.EqualFold(strings.TrimSpace(mapping.From), name) {
				if to := strings.TrimSpace(mapping.To); to != "" {
					return apply(to)
				}
			}
		}
	}
	for _, mapping := range mappings {
		from, to := strings.TrimSpace(mapping.From), strings.TrimSpace(mapping.To)
		if !mapping.Regex || from == "" || to == "" {
			continue
		}
		expression, err := regexp.Compile("(?i)" + from)
		if err == nil && (expression.MatchString(requested) || expression.MatchString(input.ModelName)) {
			return apply(to)
		}
	}
	return ""
}
