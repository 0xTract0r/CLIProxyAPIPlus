package amp

import (
	"path"
	"strings"
)

// ValidGoogleResourcePath accepts an absolute resource path beneath the fixed
// Amp Google route. A dot segment could leave the configured upstream base.
func ValidGoogleResourcePath(resource string) bool {
	return strings.HasPrefix(resource, "/") &&
		!strings.ContainsAny(resource, "\\\x00\r\n") &&
		path.Clean(resource) == strings.TrimSuffix(resource, "/")
}

// GeminiModelAction parses the ordinary Gemini methods executed locally.
func GeminiModelAction(action string) (model, method string, ok bool) {
	model, method, found := strings.Cut(strings.TrimPrefix(action, "/"), ":")
	return model, method, found && model != "" &&
		(method == "generateContent" || method == "streamGenerateContent" || method == "countTokens")
}

// GoogleInferenceAction extracts a model action from the Google resource path.
// Other actions retain their original path and use the authorized upstream.
func GoogleInferenceAction(resource string) (string, bool) {
	_, action, found := strings.Cut(resource, "/models/")
	if !found {
		return "", false
	}
	_, _, found = GeminiModelAction(action)
	return action, found
}
