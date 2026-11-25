package auth

import (
	"regexp"
	"strings"
)

// ClaudeDeviceIDMetadataKey names a field retained in older credential files.
const ClaudeDeviceIDMetadataKey = "claude_device_id"

// ClaudeDeviceIDAttributeKey identifies an inactive runtime attribute on previously loaded records.
const ClaudeDeviceIDAttributeKey = "claude_device_id"

// FarmContainerAliveAtMetadataKey names the persisted timestamp in older credential files.
const FarmContainerAliveAtMetadataKey = "farm_container_alive_at"

// FarmContainerAliveAtAttributeKey identifies the corresponding inactive runtime attribute.
const FarmContainerAliveAtAttributeKey = "farm_container_alive_at"

// claudeDeviceIDPattern validates a device_id override as a 64-char lowercase
// hex string, matching the shape of metadata.user_id.device_id on the wire.
var claudeDeviceIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IsValidClaudeDeviceID reports whether value is a well-formed 64-hex-char
// device_id suitable for persisting as an explicit override. Leading/trailing
// whitespace is tolerated; an empty (after trim) value is not valid and is
// treated by callers as "clear the override".
func IsValidClaudeDeviceID(value string) bool {
	return claudeDeviceIDPattern.MatchString(strings.TrimSpace(value))
}

func ExtractCustomHeadersFromMetadata(metadata map[string]any) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	raw, ok := metadata["headers"]
	if !ok || raw == nil {
		return nil
	}

	out := make(map[string]string)
	switch headers := raw.(type) {
	case map[string]string:
		for key, value := range headers {
			name := strings.TrimSpace(key)
			if name == "" {
				continue
			}
			val := strings.TrimSpace(value)
			if val == "" {
				continue
			}
			out[name] = val
		}
	case map[string]any:
		for key, value := range headers {
			name := strings.TrimSpace(key)
			if name == "" {
				continue
			}
			rawVal, ok := value.(string)
			if !ok {
				continue
			}
			val := strings.TrimSpace(rawVal)
			if val == "" {
				continue
			}
			out[name] = val
		}
	default:
		return nil
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

func ApplyCustomHeadersFromMetadata(auth *Auth) {
	if auth == nil || len(auth.Metadata) == 0 {
		return
	}
	headers := ExtractCustomHeadersFromMetadata(auth.Metadata)
	if len(headers) == 0 {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	for name, value := range headers {
		auth.Attributes["header:"+name] = value
	}
}

func ApplyRuntimeFieldsFromMetadata(auth *Auth) {
	if auth == nil {
		return
	}
	if strings.TrimSpace(auth.ProxyURL) == "" {
		if proxyURL, ok := auth.Metadata["proxy_url"].(string); ok {
			auth.ProxyURL = strings.TrimSpace(proxyURL)
		}
	}
	// Older files can retain inactive metadata. It must not become live SDK state.
	delete(auth.Attributes, ClaudeDeviceIDAttributeKey)
	delete(auth.Attributes, FarmContainerAliveAtAttributeKey)
}

func HasStructuredAccountSettingsMetadata(auth *Auth) bool {
	if auth == nil || len(auth.Metadata) == 0 {
		return false
	}
	raw, ok := auth.Metadata["account_settings"]
	if !ok || raw == nil {
		return false
	}
	switch value := raw.(type) {
	case map[string]any:
		return len(value) > 0
	case map[string]string:
		return len(value) > 0
	default:
		return true
	}
}
