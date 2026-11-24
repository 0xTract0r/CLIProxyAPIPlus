package executor

import (
	"net/http"

	"strings"
	"testing"
)

func TestCodexTerminalFailureStatus_ImageGenerationUserErrorMapsToBadRequest(t *testing.T) {
	failedEvent := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"type":"image_generation_user_error","code":"invalid_value","message":"Complexity 'medium' is not supported for this image model.","param":"complexity"}}}`)
	errTerminal, _, ok := codexTerminalFailureErrWithCooling(failedEvent, false)
	if !ok {
		t.Fatal("expected terminal error for response.failed")
	}
	if !strings.Contains(errTerminal.Error(), "Complexity 'medium' is not supported for this image model.") {
		t.Fatalf("unexpected error message: %v", errTerminal)
	}
	if !strings.Contains(errTerminal.Error(), "/v1/images/generations") || !strings.Contains(errTerminal.Error(), "gpt-5.4-mini") {
		t.Fatalf("expected remediation guidance in error message, got: %v", errTerminal)
	}
	if got := errTerminal.StatusCode(); got != http.StatusBadRequest {
		t.Fatalf("StatusCode() = %d, want %d (%s)", got, http.StatusBadRequest, errTerminal.Error())
	}
}
