package auth

import (
	"strings"
	"time"
)

const warmupDeferredToolsNotice = `The following deferred tools are now available via ToolSearch. Their schemas are NOT loaded — calling them directly will fail with InputValidationError. Use ToolSearch with query "select:<name>[,<name>...]" to load tool schemas before calling them:`
const warmupSkillsNotice = "The following skills are available for use with the Skill tool:"

// Recognize a bounded CLI notice grammar, not arbitrary text ending in a date.
// Descriptions are opaque catalog data; structural recognition is not proof of
// their provenance or meaning. The request itself is never edited or executed.
func warmupAuxiliaryMessage(value any) bool {
	if warmupDateMessage(value) {
		return true
	}
	message, ok := value.(map[string]any)
	if !ok || !warmupOnlyKeys(message, "role", "content") || message["role"] != "system" {
		return false
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) != 1 || !warmupIdentityText(content) {
		return false
	}
	text := content[0].(map[string]any)["text"].(string)
	sections := strings.Split(text, "\n\n")
	if len(sections) < 2 || len(sections) > 4 {
		return false
	}
	const dateLayout = "Today's date is 2006-01-02."
	date := sections[len(sections)-1]
	if len(date) != len(dateLayout) {
		return false
	}
	if _, err := time.Parse(dateLayout, date); err != nil {
		return false
	}
	switch len(sections) {
	case 2:
		return warmupToolCatalog(sections[0])
	case 3:
		return sections[0] == warmupSkillsNotice && warmupSkillCatalog(sections[1])
	case 4:
		return warmupToolCatalog(sections[0]) && sections[1] == warmupSkillsNotice && warmupSkillCatalog(sections[2])
	}
	return false
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
