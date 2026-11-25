package amp

import "testing"

func TestGoogleInferenceAction(t *testing.T) {
	for _, tc := range []struct {
		resource string
		action   string
	}{
		{"/publishers/google/models/gemini-pro:generateContent", "gemini-pro:generateContent"},
		{"/projects/test/locations/test/publishers/google/models/test/model:streamGenerateContent", "test/model:streamGenerateContent"},
		{"/models/gemini-pro:countTokens", "gemini-pro:countTokens"},
		{"/projects/test/locations/test/batchPredictionJobs", ""},
		{"/projects/test/models/gemini-pro:upload", ""},
		{"/invalid/path", ""},
	} {
		t.Run(tc.resource, func(t *testing.T) {
			if action, found := GoogleInferenceAction(tc.resource); found != (tc.action != "") || (found && action != tc.action) {
				t.Errorf("action = %q (found %v), want %q", action, found, tc.action)
			}
		})
	}
}

func TestValidGoogleResourcePath(t *testing.T) {
	for _, value := range []string{"/models", "/projects/test/locations/test/files", "/batches/jobs/", "/models/test:upload"} {
		if !ValidGoogleResourcePath(value) {
			t.Errorf("valid resource rejected: %q", value)
		}
	}
	for _, value := range []string{"", "projects/test/files", "/projects/../private", "//projects/test", "/projects\\test", "/projects/./test", "/projects//test"} {
		if ValidGoogleResourcePath(value) {
			t.Errorf("invalid resource accepted: %q", value)
		}
	}
}
