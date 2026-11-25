package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
)

// DecodeCredentialMetadata parses a single JSON credential without rounding
// numbers whose exact decimal representation can matter to file owners.
func DecodeCredentialMetadata(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var metadata map[string]any
	if err := decoder.Decode(&metadata); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("unexpected data after credential metadata")
	}
	restoreRoundTripCredentialNumbers(metadata)
	return metadata, nil
}

// Keep the ordinary float64 representation when it can write the original
// decimal back. Leaf numbers that would lose digits keep their JSON spelling.
func restoreRoundTripCredentialNumbers(value any) any {
	switch v := value.(type) {
	case json.Number:
		f, err := v.Float64()
		if err == nil && (strconv.FormatFloat(f, 'g', -1, 64) == v.String() ||
			strconv.FormatFloat(f, 'f', -1, 64) == v.String()) {
			return f
		}
	case map[string]any:
		for k, item := range v {
			v[k] = restoreRoundTripCredentialNumbers(item)
		}
	case []any:
		for i, item := range v {
			v[i] = restoreRoundTripCredentialNumbers(item)
		}
	}
	return value
}

// CanonicalCredentialMetadataKey returns the canonical snake_case name for
// credential metadata keys that previously also accepted config-style aliases.
func CanonicalCredentialMetadataKey(key string) string {
	switch key {
	case "api-key":
		return "api_key"
	case "base-url":
		return "base_url"
	case "disable-cooling":
		return "disable_cooling"
	case "excluded-models":
		return "excluded_models"
	case "fingerprint-profile":
		return "fingerprint_profile"
	case "model-aliases":
		return "model_aliases"
	case "openapi-location":
		return "openapi_location"
	case "openapi-models":
		return "openapi_models"
	case "proxy-url":
		return "proxy_url"
	case "request-retry":
		return "request_retry"
	case "request-scoped-errors":
		return "request_scoped_errors"
	case "tool-prefix-disabled":
		return "tool_prefix_disabled"
	default:
		return key
	}
}

// NormalizeCredentialMetadata rewrites recognized legacy keys to their
// canonical snake_case names. An explicitly present canonical value wins.
func NormalizeCredentialMetadata(metadata map[string]any) {
	for key, value := range metadata {
		canonical := CanonicalCredentialMetadataKey(key)
		if canonical == key {
			continue
		}
		if _, exists := metadata[canonical]; !exists {
			metadata[canonical] = value
		}
		delete(metadata, key)
	}
}
