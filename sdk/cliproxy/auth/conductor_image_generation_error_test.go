package auth

import (
	"net/http"
	"testing"
)

// TestIsRequestInvalidError_ImageGenerationUserError 验证：HTTP 400 且 message 含
// image_generation_user_error 时归类为请求级不可重试（返回 true），不触发账号轮换。
func TestIsRequestInvalidError_ImageGenerationUserError(t *testing.T) {
	err := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"error":{"type":"image_generation_user_error","param":"tools","code":"invalid_value","message":"The model 'gpt-image-2' does not exist."}}`,
	}
	if !isRequestInvalidError(err) {
		t.Fatalf("expected image_generation_user_error 400 to be request-level (true)")
	}
}

// TestIsRequestInvalidError_PlainBadRequestStillNotInvalid 验证：不含已知请求级标记的
// 普通 400 维持原行为（返回 false），不被新逻辑误伤。
func TestIsRequestInvalidError_PlainBadRequestStillNotInvalid(t *testing.T) {
	err := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"error":{"type":"server_overloaded","message":"please retry"}}`,
	}
	if isRequestInvalidError(err) {
		t.Fatalf("expected plain 400 without request-level markers to stay false")
	}
}

// TestIsRequestInvalidError_ExistingMarkersUnchanged 验证既有请求级标记行为不变。
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
