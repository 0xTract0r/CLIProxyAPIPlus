package auth

import (
	"strings"
	"time"
)

const warmupDeferredToolsNotice = `The following deferred tools are now available via ToolSearch. Their schemas are NOT loaded — calling them directly will fail with InputValidationError. Use ToolSearch with query "select:<name>[,<name>...]" to load tool schemas before calling them:`
const warmupSkillsNotice = "The following skills are available for use with the Skill tool:"

const warmupAuxiliaryMaxBytes = 128 << 10

// Complete notice units can move between text blocks or system messages.
// A catalog header and its entries must stay in the same complete text unit.
// Descriptions are opaque data, not evidence of semantic trust or provenance.
func warmupAuxiliaryMessages(messages []any) bool {
	if len(messages) == 0 || len(messages) > 8 {
		return false
	}
	seen, size, blocks := uint8(0), 0, 0
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || !warmupOnlyKeys(message, "role", "content") || message["role"] != "system" || !warmupIdentityText(message["content"]) {
			return false
		}
		texts := warmupTextBlocks(message["content"])
		if len(texts) == 0 {
			return false
		}
		for _, text := range texts {
			size += len(text)
			blocks++
			if size > warmupAuxiliaryMaxBytes || blocks > 8 {
				return false
			}
			units, ok := warmupNoticeUnits(text)
			if !ok || seen&units != 0 {
				return false
			}
			seen |= units
		}
	}
	return seen != 0
}

func warmupAuxiliaryMessage(value any) bool {
	return warmupAuxiliaryMessages([]any{value})
}

func warmupNoticeUnits(text string) (uint8, bool) {
	if len(text) == 0 || len(text) > warmupAuxiliaryMaxBytes {
		return 0, false
	}
	sections := strings.Split(text, "\n\n")
	if len(sections) > 4 {
		return 0, false
	}
	seen := uint8(0)
	for i := 0; i < len(sections); i++ {
		unit := uint8(0)
		switch {
		case warmupToolCatalog(sections[i]):
			unit = 1
		case sections[i] == warmupSkillsNotice:
			if i+1 >= len(sections) || !warmupSkillCatalog(sections[i+1]) {
				return 0, false
			}
			i++
			unit = 2
		default:
			const layout = "Today's date is 2006-01-02."
			if len(sections[i]) != len(layout) {
				return 0, false
			}
			if _, err := time.Parse(layout, sections[i]); err != nil {
				return 0, false
			}
			unit = 4
		}
		if seen&unit != 0 {
			return 0, false
		}
		seen |= unit
	}
	return seen, seen != 0
}

func warmupTextBlocks(content any) []string {
	if text, ok := content.(string); ok {
		return []string{text}
	}
	blocks, _ := content.([]any)
	texts := make([]string, 0, len(blocks))
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		text, _ := block["text"].(string)
		texts = append(texts, text)
	}
	return texts
}

func warmupToolCatalog(section string) bool {
	lines := strings.Split(section, "\n")
	if len(lines) < 2 || lines[0] != warmupDeferredToolsNotice {
		return false
	}
	seen := make(map[string]bool, len(lines)-1)
	for _, name := range lines[1:] {
		if !warmupCatalogName(name, false) || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}

func warmupSkillCatalog(section string) bool {
	seen := make(map[string]bool)
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "- ") {
			name, description, ok := strings.Cut(line[2:], ": ")
			if !ok || !warmupCatalogName(name, true) || seen[name] || strings.TrimSpace(description) == "" {
				return false
			}
			seen[name] = true
			continue
		}
		// The observed CLI permits unindented continuation lines. They must
		// belong to an entry, without introducing another catalog/date section.
		if len(seen) == 0 || strings.TrimSpace(line) == "" || line == warmupDeferredToolsNotice || line == warmupSkillsNotice || strings.HasPrefix(line, "Today's date is ") {
			return false
		}
	}
	return len(seen) > 0
}

func warmupCatalogName(name string, namespaced bool) bool {
	if name == "" {
		return false
	}
	if namespaced {
		for _, segment := range strings.Split(name, ":") {
			if !warmupCatalogName(segment, false) {
				return false
			}
		}
		return true
	}
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}
