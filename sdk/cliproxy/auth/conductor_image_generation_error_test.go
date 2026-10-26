package auth

import (
	"net/http"
	"testing"
)

// Image generation user errors are request faults and must not rotate credentials.
func TestIsRequestInvalidError_ImageGenerationUserError(t *testing.T) {
	err := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"error":{"type":"image_generation_user_error","param":"tools","code":"invalid_value","message":"The model 'gpt-image-2' does not exist."}}`,
	}
	if !isRequestInvalidError(err) {
		t.Fatalf("expected image_generation_user_error 400 to be request-level (true)")
	}
}

// Plain 400s without recognized request-fault markers retain credential failover.
func TestIsRequestInvalidError_PlainBadRequestStillNotInvalid(t *testing.T) {
	err := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"error":{"type":"server_overloaded","message":"please retry"}}`,
	}
	if isRequestInvalidError(err) {
		t.Fatalf("expected plain 400 without request-level markers to stay false")
	}
}

// Existing explicit request-fault markers remain non-retryable.
func TestIsRequestInvalidError_ExistingMarkersUnchanged(t *testing.T) {
	cases := []string{
		`{"error":{"type":"invalid_request_error","message":"bad"}}`,
		`{"error":{"status":"INVALID_ARGUMENT"}}`,
		`{"error":{"status":"FAILED_PRECONDITION"}}`,
	}
	for _, msg := range cases {
		err := &Error{HTTPStatus: http.StatusBadRequest, Message: msg}
		if !isRequestInvalidError(err) {
			t.Fatalf("expected existing request-level marker to stay true: %s", msg)
		}
	}
}

// Plain overload 400 remains retryable in the fork; explicit upstream request
// identifiers stop rotation, while payment/quota statuses still belong to auth.
func TestIsRequestInvalidError_UpstreamMarkersAndCredentialFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		message string
		want    bool
	}{
		{"plain overload", 400, `{"error":{"type":"server_overloaded","message":"please retry"}}`, false},
		{"context marker", 400, `{"error":{"code":"context_length_exceeded"}}`, true},
		{"continuation marker", 400, `{"error":{"code":"previous_response_not_found"}}`, true},
		{"cyber marker", 400, `{"error":{"code":"cyber_policy"}}`, true},
		{"payment wins", 402, `{"error":{"type":"invalid_request_error"}}`, false},
		{"quota wins", 429, `{"error":{"type":"invalid_request_error"}}`, false},
		{"model capability", 400, `{"error":{"code":"model_not_found"}}`, false},
		{"unknown upstream internal error", 500, `{"error":{"code":500,"status":"UNKNOWN","message":"Internal error encountered."}}`, false},
		{"explicit request fault behind unknown status", 500, `{"error":{"type":"invalid_request_error","status":"UNKNOWN","message":"bad input"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRequestInvalidError(&Error{HTTPStatus: tc.status, Message: tc.message}); got != tc.want {
				t.Fatalf("request invalid=%v, want %v", got, tc.want)
			}
		})
	}
}
