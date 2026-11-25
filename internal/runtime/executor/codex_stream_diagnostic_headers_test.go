package executor

import (
	"net/http"
	"reflect"
	"testing"
)

func TestOrdinaryCodexStreamDiagnosticHeadersExcludeCredentials(t *testing.T) {
	request := http.Header{
		"Accept": {"text/event-stream"}, "Content-Type": {"application/json"}, "Content-Encoding": {"zstd"},
	}
	for key, value := range map[string]string{"Authorization": "Bearer x", "Proxy-Authorization": "Basic x", "ChatGPT-Account-ID": "fixture-account", "Cookie": "fixture-cookie", "X-API-Key": "fixture-api-key", "Api-Key": "fixture-other-api-key"} {
		request.Set(key, value)
	}
	original := request.Clone()
	want := http.Header{"Accept": {"text/event-stream"}, "Content-Type": {"application/json"}, "Content-Encoding": {"zstd"}}
	got := codexStreamDiagnosticHeaders(request)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostic names or formats differ from the approved fields")
	}
	got.Values("Accept")[0] = "sentinel"
	if !reflect.DeepEqual(request, original) {
		t.Fatal("diagnostic copy mutated the real request headers")
	}
}
