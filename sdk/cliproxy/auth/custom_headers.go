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

// FarmContainerAliveAtMetadataKey is the persisted auth.Metadata key for the
// farm container-liveness heartbeat: an RFC3339 UTC timestamp the farm
// orchestrator refreshes (via PATCH /v0/management/auth-files/fields) while the
// account's bound container is alive. An empty string clears it (the account is
// then treated as not-alive by the container-liveness sub-gate). This is a
// cross-slice contract field name shared with the orchestrator; do not rename.
const FarmContainerAliveAtMetadataKey = "farm_container_alive_at"

// FarmContainerAliveAtAttributeKey is the live runtime mirror of
// FarmContainerAliveAtMetadataKey, hydrated into Auth.Attributes by
// ApplyRuntimeFieldsFromMetadata (same mechanism as claude_device_id). The
// container-liveness sub-gate (authContainerRecentlyAlive) reads the heartbeat
// from Auth.Attributes rather than Auth.Metadata so it stays consistent with the
// other hydrated runtime fields the selector relies on.
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
	if auth == nil || len(auth.Metadata) == 0 {
		return
	}
	if strings.TrimSpace(auth.ProxyURL) == "" {
		if proxyURL, ok := auth.Metadata["proxy_url"].(string); ok {
			auth.ProxyURL = strings.TrimSpace(proxyURL)
		}
	}
	applyClaudeDeviceIDFromMetadata(auth)
	applyFarmAliveAtFromMetadata(auth)
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

// applyFarmAliveAtFromMetadata mirrors the persisted farm container-liveness
// heartbeat (FarmContainerAliveAtMetadataKey) into Attributes so the
// container-liveness sub-gate (authContainerRecentlyAlive) can read it at
// selection time without touching Metadata directly, exactly like
// applyClaudeDeviceIDFromMetadata does for the device_id override. It mirrors a
// non-empty string value verbatim, and clears any stale mirrored value when the
// persisted heartbeat is missing, empty (an explicit "clear"), or not a string,
// so a cleared heartbeat reliably falls back to not-alive on a live,
// already-hydrated Auth object (e.g. a management PATCH followed by
// Manager.Update, which re-runs this hydration on the same pointer rather than a
// freshly loaded one). RFC3339 shape is NOT validated here — write-side
// validation lives in the management PATCH guard (validateFarmAliveAtPatch) and
// freshness/parse validation lives in the gate predicate; hydration only mirrors
// bytes so the two mirrors (device_id and alive-at) stay symmetric.
func applyFarmAliveAtFromMetadata(auth *Auth) {
	raw, ok := auth.Metadata[FarmContainerAliveAtMetadataKey]
	if !ok {
		if auth.Attributes != nil {
			delete(auth.Attributes, FarmContainerAliveAtAttributeKey)
		}
		return
	}
	str, isString := raw.(string)
	trimmed := strings.TrimSpace(str)
	if !isString || trimmed == "" {
		if auth.Attributes != nil {
			delete(auth.Attributes, FarmContainerAliveAtAttributeKey)
		}
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[FarmContainerAliveAtAttributeKey] = trimmed
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
