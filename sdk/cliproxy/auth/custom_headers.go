package auth

import (
	"regexp"
	"strings"
)

// ClaudeDeviceIDMetadataKey is the persisted auth.Metadata key for an
// operator-supplied explicit Claude device_id override. When present and
// valid it takes precedence over the per-account synthetic value derived by
// helps.SyntheticDeviceID; when absent, empty, or invalid the synthetic
// fallback is used instead.
const ClaudeDeviceIDMetadataKey = "claude_device_id"

// ClaudeDeviceIDAttributeKey is the live runtime mirror of
// ClaudeDeviceIDMetadataKey, hydrated into Auth.Attributes by
// ApplyRuntimeFieldsFromMetadata (same mechanism used for proxy_url). Runtime
// code that needs the override value at request time reads it from
// Auth.Attributes rather than Auth.Metadata so it stays consistent with the
// other hydrated runtime fields.
const ClaudeDeviceIDAttributeKey = "claude_device_id"

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
	if auth == nil || len(auth.Metadata) == 0 {
		return
	}
	if strings.TrimSpace(auth.ProxyURL) == "" {
		if proxyURL, ok := auth.Metadata["proxy_url"].(string); ok {
			auth.ProxyURL = strings.TrimSpace(proxyURL)
		}
	}
	applyClaudeDeviceIDFromMetadata(auth)
}

// applyClaudeDeviceIDFromMetadata mirrors a valid ClaudeDeviceIDMetadataKey
// override into Attributes so runtime code (helps.SyntheticDeviceID) can read
// it without touching Metadata directly. It also clears any stale mirrored
// value when the persisted override is missing, empty, or invalid, so an
// operator clearing the field (or an invalid value) reliably falls back to
// the synthetic derivation instead of leaving a previous run's override
// active on a live, already-hydrated Auth object (e.g. management API patch
// followed by Manager.Update, which re-runs this hydration on the same
// pointer rather than a freshly loaded one).
func applyClaudeDeviceIDFromMetadata(auth *Auth) {
	raw, ok := auth.Metadata[ClaudeDeviceIDMetadataKey]
	if !ok {
		if auth.Attributes != nil {
			delete(auth.Attributes, ClaudeDeviceIDAttributeKey)
		}
		return
	}
	str, isString := raw.(string)
	trimmed := strings.TrimSpace(str)
	if !isString || !IsValidClaudeDeviceID(trimmed) {
		if auth.Attributes != nil {
			delete(auth.Attributes, ClaudeDeviceIDAttributeKey)
		}
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[ClaudeDeviceIDAttributeKey] = trimmed
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
