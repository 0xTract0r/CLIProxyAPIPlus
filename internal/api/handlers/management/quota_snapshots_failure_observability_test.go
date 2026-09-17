package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

// TestQuotaErrorFieldsFromBodyExtractsOnlyErrorTypeAndMessage is the allow-list
// guard for the non-2xx body read. A quota error body can echo organization
// uuids, account emails or tokens; only error.type and error.message may be
// kept, everything else must be discarded. Note this guards the allow-list, not
// scrubbing: content inside the two kept string fields is persisted verbatim.
func TestQuotaErrorFieldsFromBodyExtractsOnlyErrorTypeAndMessage(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"error": {
			"type": "rate_limit_error",
			"message": "Number of requests has exceeded your rate limit",
			"organization_uuid": "11111111-2222-3333-4444-555555555555",
			"details": {"email": "leak-detail@example.com"}
		},
		"organization": {"uuid": "99999999-8888-7777-6666-555555555555", "name": "Secret Org"},
		"account": {"email": "leak-account@example.com"},
		"request_id": "req_leak_marker"
	}`)

	errorType, errorMessage := quotaErrorFieldsFromBody(body)
	if errorType != "rate_limit_error" {
		t.Fatalf("error type = %q, want %q", errorType, "rate_limit_error")
	}
	if errorMessage != "Number of requests has exceeded your rate limit" {
		t.Fatalf("error message = %q, want the upstream message", errorMessage)
	}
	for _, forbidden := range []string{
		"11111111-2222-3333-4444-555555555555",
		"99999999-8888-7777-6666-555555555555",
		"leak-detail@example.com",
		"leak-account@example.com",
		"Secret Org",
		"req_leak_marker",
	} {
		if strings.Contains(errorType, forbidden) || strings.Contains(errorMessage, forbidden) {
			t.Fatalf("extracted fields leaked %q: type=%q message=%q", forbidden, errorType, errorMessage)
		}
	}
}

// TestQuotaErrorFieldsFromBodyDropsNonStringValues closes the hole the
// string-only fixtures above cannot see: gjson's Result.String() returns the
// RAW JSON text for object/array results, so reading error.message without a
// type check copies a whole nested object through the two-field allow-list
// verbatim. A body that puts an object at error.message (or an array at
// error.type) would then persist emails / org uuids into auth metadata even
// though neither field name is on the allow-list. Truncation does not help:
// leak markers are short.
func TestQuotaErrorFieldsFromBodyDropsNonStringValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{
			name: "object error.message",
			body: `{"error":{"type":"x","message":{"detail":"boom","account_email":"leak@example.com","org_uuid":"11111111-2222-3333-4444-555555555555"}}}`,
		},
		{
			name: "array error.type",
			body: `{"error":{"type":["a","leak2@example.com"],"message":"ok"}}`,
		},
		{
			name: "both non-string",
			body: `{"error":{"type":{"nested":"leak3@example.com"},"message":["leak4@example.com"]}}`,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			errorType, errorMessage := quotaErrorFieldsFromBody([]byte(tt.body))
			for _, forbidden := range []string{
				"leak@example.com",
				"leak2@example.com",
				"leak3@example.com",
				"leak4@example.com",
				"11111111-2222-3333-4444-555555555555",
				"account_email",
				"org_uuid",
				"{",
				"[",
			} {
				if strings.Contains(errorType, forbidden) {
					t.Fatalf("non-string error.type leaked %q: %q", forbidden, errorType)
				}
				if strings.Contains(errorMessage, forbidden) {
					t.Fatalf("non-string error.message leaked %q: %q", forbidden, errorMessage)
				}
			}
		})
	}

	// Explicit per-field expectations: the non-string field is empty, a sibling
	// string field is still extracted normally.
	errorType, errorMessage := quotaErrorFieldsFromBody([]byte(cases[0].body))
	if errorType != "x" {
		t.Fatalf("error type = %q, want the sibling string value %q", errorType, "x")
	}
	if errorMessage != "" {
		t.Fatalf("object error.message = %q, want empty (dropped)", errorMessage)
	}
	errorType, errorMessage = quotaErrorFieldsFromBody([]byte(cases[1].body))
	if errorType != "" {
		t.Fatalf("array error.type = %q, want empty (dropped)", errorType)
	}
	if errorMessage != "ok" {
		t.Fatalf("error message = %q, want the sibling string value %q", errorMessage, "ok")
	}

	// Scalar non-strings are dropped too: the fields are documented as strings,
	// so a number / bool / null must not be coerced into persisted text.
	for _, body := range []string{
		`{"error":{"type":123,"message":456}}`,
		`{"error":{"type":true,"message":false}}`,
		`{"error":{"type":null,"message":null}}`,
	} {
		gotType, gotMessage := quotaErrorFieldsFromBody([]byte(body))
		if gotType != "" || gotMessage != "" {
			t.Fatalf("quotaErrorFieldsFromBody(%s) = (%q, %q), want empty fields", body, gotType, gotMessage)
		}
	}
}

// TestQuotaErrorFieldsFromBodyTolerantOfNonJSONAndTruncatesLongFields pins the
// degradation contract: a missing/HTML/garbage body yields empty fields rather
// than a panic, and an oversized message is truncated.
func TestQuotaErrorFieldsFromBodyTolerantOfNonJSONAndTruncatesLongFields(t *testing.T) {
	t.Parallel()

	for _, body := range [][]byte{nil, {}, []byte("<html>502 Bad Gateway</html>"), []byte(`{"error":`)} {
		errorType, errorMessage := quotaErrorFieldsFromBody(body)
		if errorType != "" || errorMessage != "" {
			t.Fatalf("quotaErrorFieldsFromBody(%q) = (%q, %q), want empty fields", string(body), errorType, errorMessage)
		}
	}

	long := strings.Repeat("x", quotaErrorFieldMaxRunes+500)
	_, errorMessage := quotaErrorFieldsFromBody([]byte(`{"error":{"message":"` + long + `"}}`))
	if len([]rune(errorMessage)) != quotaErrorFieldMaxRunes+3 {
		t.Fatalf("truncated message rune length = %d, want %d (limit + ellipsis)", len([]rune(errorMessage)), quotaErrorFieldMaxRunes+3)
	}
	if !strings.HasSuffix(errorMessage, "...") {
		t.Fatalf("truncated message = %q, want an ellipsis suffix", errorMessage)
	}
}

// TestQuotaRetryAfterFromHeaderParsesSecondsAndHTTPDate covers both Retry-After
// forms plus the missing/malformed degradation to a zero value.
func TestQuotaRetryAfterFromHeaderParsesSecondsAndHTTPDate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	t.Run("delay seconds", func(t *testing.T) {
		got := quotaRetryAfterFromHeader(http.Header{"Retry-After": []string{"120"}}, now)
		if got != 2*time.Minute {
			t.Fatalf("retry after = %v, want 2m", got)
		}
	})

	t.Run("delay seconds with surrounding space", func(t *testing.T) {
		got := quotaRetryAfterFromHeader(http.Header{"Retry-After": []string{"  45 "}}, now)
		if got != 45*time.Second {
			t.Fatalf("retry after = %v, want 45s", got)
		}
	})

	t.Run("http date", func(t *testing.T) {
		resetAt := now.Add(90 * time.Second)
		header := http.Header{"Retry-After": []string{resetAt.Format(http.TimeFormat)}}
		got := quotaRetryAfterFromHeader(header, now)
		if got != 90*time.Second {
			t.Fatalf("retry after = %v, want 90s", got)
		}
	})

	t.Run("degrades to zero", func(t *testing.T) {
		cases := []struct {
			name   string
			header http.Header
		}{
			{name: "nil header", header: nil},
			{name: "absent", header: http.Header{}},
			{name: "empty value", header: http.Header{"Retry-After": []string{"   "}}},
			{name: "malformed", header: http.Header{"Retry-After": []string{"soon-ish"}}},
			{name: "negative seconds", header: http.Header{"Retry-After": []string{"-30"}}},
			{name: "zero seconds", header: http.Header{"Retry-After": []string{"0"}}},
			{name: "past http date", header: http.Header{"Retry-After": []string{now.Add(-time.Hour).Format(http.TimeFormat)}}},
		}
		for _, tt := range cases {
			t.Run(tt.name, func(t *testing.T) {
				if got := quotaRetryAfterFromHeader(tt.header, now); got != 0 {
					t.Fatalf("retry after = %v, want 0", got)
				}
			})
		}
	})
}

// TestQuotaHTTPErrorMessageFormatUnchanged is a regression guard. The persisted
// quota_refresh_error text is how quotaSnapshotLegacyReauthRequired recognizes
// a legacy 401/403 failure and how quotaSnapshotErrorClass falls back to
// http_status, so the observation fields must never change this string.
func TestQuotaHTTPErrorMessageFormatUnchanged(t *testing.T) {
	t.Parallel()

	bare := &quotaHTTPError{StatusCode: http.StatusTooManyRequests}
	if got := bare.Error(); got != "quota endpoint returned non-success status 429" {
		t.Fatalf("Error() = %q, want the historical format", got)
	}

	enriched := &quotaHTTPError{
		StatusCode:   http.StatusTooManyRequests,
		RetryAfter:   5 * time.Minute,
		ErrorType:    "rate_limit_error",
		ErrorMessage: "slow down",
	}
	if got := enriched.Error(); got != bare.Error() {
		t.Fatalf("Error() with observation = %q, want identical to %q", got, bare.Error())
	}
	if got := quotaSnapshotErrorClass(enriched, quotaRefreshStatusError); got != "http_status" {
		t.Fatalf("error class = %q, want http_status", got)
	}
}

// TestQuotaReauthRequiredErrorMessageStaysSanitized asserts the reauth error
// keeps leaking nothing even though it now carries the observation.
func TestQuotaReauthRequiredErrorMessageStaysSanitized(t *testing.T) {
	t.Parallel()

	err := &quotaReauthRequiredError{
		Provider:     "claude",
		StatusCode:   http.StatusUnauthorized,
		RetryAfter:   time.Minute,
		ErrorType:    "authentication_error",
		ErrorMessage: "Invalid authentication credentials",
	}
	got := err.Error()
	if got != claudeQuotaCredentialUnauthorizedMessage {
		t.Fatalf("Error() = %q, want the sanitized reauth message", got)
	}
	for _, forbidden := range []string{"401", "authentication_error", "Invalid authentication credentials", "1m"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("reauth error leaked %q: %q", forbidden, got)
		}
	}
}

// TestQuotaRefreshObservabilityMetadataKeysAreClearedOnReauth pins the
// registration of the two new keys in reauthRuntimeMetadataKeys. A stale
// observation surviving a re-auth would describe an already-replaced
// credential.
func TestQuotaRefreshObservabilityMetadataKeysAreClearedOnReauth(t *testing.T) {
	t.Parallel()

	for _, key := range []string{quotaRefreshHTTPStatusMetadataKey, quotaRefreshRetryAfterMetadataKey} {
		if _, ok := reauthRuntimeMetadataKeys[key]; !ok {
			t.Fatalf("reauthRuntimeMetadataKeys is missing %q; a stale quota observation would survive re-auth", key)
		}
		if !isReauthRuntimeMetadataKey(key) {
			t.Fatalf("isReauthRuntimeMetadataKey(%q) = false, want true", key)
		}
	}
	if quotaRefreshHTTPStatusMetadataKey != "quota_refresh_http_status" {
		t.Fatalf("http status key = %q, want quota_refresh_http_status", quotaRefreshHTTPStatusMetadataKey)
	}
	if quotaRefreshRetryAfterMetadataKey != "quota_refresh_retry_after_seconds" {
		t.Fatalf("retry after key = %q, want quota_refresh_retry_after_seconds", quotaRefreshRetryAfterMetadataKey)
	}
}

// TestQuotaSnapshotRateLimitPersistsStatusAndRetryAfter is the end-to-end case:
// a 429 with a Retry-After header and an error body must persist the status and
// the retry hint, surface them on the refresh result, and still leave the
// tokenless error message and the existing scheduling behaviour untouched.
func TestQuotaSnapshotRateLimitPersistsStatusAndRetryAfter(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{
		provider: "claude",
		responses: map[string]quotaSnapshotTestResponse{
			"https://api.anthropic.com/api/oauth/profile": {
				statusCode: http.StatusTooManyRequests,
				header:     http.Header{"Retry-After": []string{"300"}},
				body:       `{"error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"},"organization":{"uuid":"leak-uuid-marker"},"account":{"email":"leak@example.com"}}`,
			},
		},
	}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ProxyURL: "http://test-proxy:8080",
		ID:       "claude-rate-limited",
		Provider: "claude",
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"auth_id":"claude-rate-limited"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}

	result := quotaRefreshResultForAuth(t, decodeQuotaSnapshotPayload(t, rec), "claude-rate-limited")
	if result.Status != quotaRefreshStatusError {
		t.Fatalf("result status = %q, want %q (429 must not be treated as reauth)", result.Status, quotaRefreshStatusError)
	}
	if result.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("result http_status = %d, want 429", result.HTTPStatus)
	}
	if result.RetryAfterSeconds != 300 {
		t.Fatalf("result retry_after_seconds = %d, want 300", result.RetryAfterSeconds)
	}
	if result.ProviderErrorType != "rate_limit_error" {
		t.Fatalf("result provider_error_type = %q, want rate_limit_error", result.ProviderErrorType)
	}
	if result.ErrorClass != "http_status" {
		t.Fatalf("result error_class = %q, want http_status", result.ErrorClass)
	}

	updated, ok := manager.GetByID("claude-rate-limited")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusError {
		t.Fatalf("persisted quota status = %q, want %q", got, quotaRefreshStatusError)
	}
	if got := metadataInt(updated.Metadata, quotaRefreshHTTPStatusMetadataKey); got != http.StatusTooManyRequests {
		t.Fatalf("persisted %s = %d, want 429", quotaRefreshHTTPStatusMetadataKey, got)
	}
	if got := metadataInt(updated.Metadata, quotaRefreshRetryAfterMetadataKey); got != 300 {
		t.Fatalf("persisted %s = %d, want 300", quotaRefreshRetryAfterMetadataKey, got)
	}
	// A 429 must NOT be reclassified as reauth-required, and the persisted
	// message must stay the historical tokenless sentence.
	if got := metadataString(updated.Metadata, quotaRefreshErrorMetadataKey); got != "quota endpoint returned non-success status 429" {
		t.Fatalf("persisted quota error = %q, want the historical non-success sentence", got)
	}
	if quotaSnapshotImplicitRefreshSkipped(updated) {
		t.Fatal("a 429 must not make the account sticky-skipped by the background poller")
	}
	// Retry-After must not be allowed to move the next refresh: honoring it is
	// explicitly out of scope, the schedule stays policy-driven.
	next, hasNext := quotaSnapshotNextRefresh(updated)
	if !hasNext {
		t.Fatal("next refresh should still be scheduled after a failed probe")
	}
	if remaining := time.Until(next); remaining <= 5*time.Minute {
		t.Fatalf("next refresh in %v; Retry-After must not shorten the policy schedule", remaining)
	}
	// The body's uuid / email must not reach metadata or the HTTP response.
	for _, forbidden := range []string{"leak-uuid-marker", "leak@example.com"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("refresh response leaked %q: %s", forbidden, rec.Body.String())
		}
		for key, value := range updated.Metadata {
			if str, isStr := value.(string); isStr && strings.Contains(str, forbidden) {
				t.Fatalf("metadata[%q] leaked %q: %q", key, forbidden, str)
			}
		}
	}
}

// TestQuotaSnapshotFailureWithoutRetryAfterOmitsObservabilityKeys asserts the
// keys are absent (not zero-valued) when the upstream sends no Retry-After, and
// that a non-HTTP failure records no status at all.
func TestQuotaSnapshotFailureWithoutRetryAfterOmitsObservabilityKeys(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{
		provider: "codex",
		responses: map[string]quotaSnapshotTestResponse{
			"https://chatgpt.com/backend-api/wham/usage": {
				statusCode: http.StatusBadGateway,
				body:       "<html>502</html>",
			},
		},
	}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ProxyURL: "http://test-proxy:8080",
		ID:       "codex-502",
		Provider: "codex",
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)

	auth, ok := manager.GetByID("codex-502")
	if !ok {
		t.Fatal("registered auth missing")
	}
	if _, err := handler.refreshQuotaSnapshot(context.Background(), auth, defaultQuotaSnapshotTestPolicy()); err == nil {
		t.Fatal("refreshQuotaSnapshot() error = nil, want a failure")
	}

	updated, ok := manager.GetByID("codex-502")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataInt(updated.Metadata, quotaRefreshHTTPStatusMetadataKey); got != http.StatusBadGateway {
		t.Fatalf("persisted %s = %d, want 502", quotaRefreshHTTPStatusMetadataKey, got)
	}
	if _, present := updated.Metadata[quotaRefreshRetryAfterMetadataKey]; present {
		t.Fatalf("%s should be absent without a Retry-After header, got %#v", quotaRefreshRetryAfterMetadataKey, updated.Metadata[quotaRefreshRetryAfterMetadataKey])
	}
}

// TestQuotaSnapshotSuccessClearsFailureObservability asserts a recovered probe
// drops the previous failure's observation, so the keys never describe a stale
// failure.
func TestQuotaSnapshotSuccessClearsFailureObservability(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "claude"}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ProxyURL: "http://test-proxy:8080",
		ID:       "claude-recovering",
		Provider: "claude",
		Metadata: map[string]any{
			quotaRefreshStatusMetadataKey:     quotaRefreshStatusError,
			quotaRefreshErrorMetadataKey:      "quota endpoint returned non-success status 429",
			quotaRefreshHTTPStatusMetadataKey: 429,
			quotaRefreshRetryAfterMetadataKey: int64(300),
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)

	auth, ok := manager.GetByID("claude-recovering")
	if !ok {
		t.Fatal("registered auth missing")
	}
	if _, err := handler.refreshQuotaSnapshot(context.Background(), auth, defaultQuotaSnapshotTestPolicy()); err != nil {
		t.Fatalf("refreshQuotaSnapshot() error = %v, want success", err)
	}

	updated, ok := manager.GetByID("claude-recovering")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusOK {
		t.Fatalf("persisted quota status = %q, want %q", got, quotaRefreshStatusOK)
	}
	for _, key := range []string{quotaRefreshHTTPStatusMetadataKey, quotaRefreshRetryAfterMetadataKey} {
		if _, present := updated.Metadata[key]; present {
			t.Fatalf("metadata[%q] survived a successful refresh: %#v", key, updated.Metadata[key])
		}
	}
}

// TestFarmLivenessProbeWritersClearFailureObservation covers the OTHER
// quota_refresh_status writers. The success-clear test above only exercises
// refreshQuotaSnapshot; the liveness probe has its own success and
// unauthorized writers, and neither writes an observation of its own. Before
// clearQuotaFailureObservation existed, an account that 429'd and then had a
// successful probe kept status=ok alongside http_status=429 /
// retry_after=300, i.e. the management view reported a rate limit the account
// had already recovered from.
func TestFarmLivenessProbeWritersClearFailureObservation(t *testing.T) {
	staleObservation := func() map[string]any {
		return map[string]any{
			quotaRefreshStatusMetadataKey:     quotaRefreshStatusError,
			quotaRefreshErrorMetadataKey:      "quota endpoint returned non-success status 429",
			quotaRefreshHTTPStatusMetadataKey: 429,
			quotaRefreshRetryAfterMetadataKey: int64(300),
		}
	}
	seedStale := func(t *testing.T, manager *coreauth.Manager, id string) *coreauth.Auth {
		t.Helper()
		auth, ok := manager.GetByID(id)
		if !ok {
			t.Fatalf("registered auth %s missing", id)
		}
		seeded := auth.Clone()
		if seeded.Metadata == nil {
			seeded.Metadata = map[string]any{}
		}
		for key, value := range staleObservation() {
			seeded.Metadata[key] = value
		}
		if _, err := manager.Update(context.Background(), seeded); err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		current, _ := manager.GetByID(id)
		if got := metadataInt(current.Metadata, quotaRefreshHTTPStatusMetadataKey); got != 429 {
			t.Fatalf("precondition: seeded http_status = %d, want 429", got)
		}
		return current
	}
	assertCleared := func(t *testing.T, meta map[string]any) {
		t.Helper()
		for _, key := range []string{quotaRefreshHTTPStatusMetadataKey, quotaRefreshRetryAfterMetadataKey} {
			if _, present := meta[key]; present {
				t.Fatalf("metadata[%q] survived a liveness probe status rewrite: %#v", key, meta[key])
			}
		}
	}

	t.Run("probe success", func(t *testing.T) {
		t.Setenv(FarmLivenessProbeEnvVar, "true")
		t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "0")

		handler, manager, _ := newLivenessQuotaTestHandler(t)
		registerClaudeAuth(t, manager, "claude-stale-obs-ok", true)
		seeded := seedStale(t, manager, "claude-stale-obs-ok")

		// Default test executor answers 200, so this drives
		// applyLivenessProbeSuccess.
		handler.probeAccountLiveness(context.Background(), manager, seeded, defaultQuotaSnapshotTestPolicy())

		updated, ok := manager.GetByID("claude-stale-obs-ok")
		if !ok {
			t.Fatal("updated auth missing")
		}
		if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusOK {
			t.Fatalf("quota status = %q, want %q", got, quotaRefreshStatusOK)
		}
		assertCleared(t, updated.Metadata)
	})

	t.Run("probe unauthorized", func(t *testing.T) {
		t.Setenv(FarmLivenessProbeEnvVar, "true")
		t.Setenv(coreauth.FarmRequireProvisionedEnvVar, "0")

		manager := coreauth.NewManager(nil, nil, nil)
		exec := &quotaSnapshotTestExecutor{
			provider: "claude",
			responses: map[string]quotaSnapshotTestResponse{
				"https://api.anthropic.com/api/oauth/profile": {
					statusCode: http.StatusUnauthorized,
					body:       `{"error":{"type":"authentication_error","message":"invalid credentials"}}`,
				},
			},
		}
		manager.RegisterExecutor(exec)
		handler := NewHandlerWithoutConfigFilePath(nil, manager)
		registerClaudeAuth(t, manager, "claude-stale-obs-401", true)
		seeded := seedStale(t, manager, "claude-stale-obs-401")

		handler.probeAccountLiveness(context.Background(), manager, seeded, defaultQuotaSnapshotTestPolicy())

		updated, ok := manager.GetByID("claude-stale-obs-401")
		if !ok {
			t.Fatal("updated auth missing")
		}
		if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusReauthRequired {
			t.Fatalf("quota status = %q, want %q", got, quotaRefreshStatusReauthRequired)
		}
		// The 401 observation belongs to the quota poller's writer, not this one:
		// this writer must not leave the earlier 429 in place.
		assertCleared(t, updated.Metadata)
	})
}

// TestQuotaHealthBlindStampClearsFailureObservation is the missing lock on the
// FOURTH clearQuotaFailureObservation call site. The other three writers
// (refresh success, liveness probe success, liveness probe unauthorized) are
// covered above, but stampQuotaHealthBlind's clear was asserted nowhere:
// deleting that single call left the whole package green, so a later refactor
// could silently drop it. health_blind means "not probed at all", so an account
// that goes health-blind after a 429 must not keep advertising http_status=429 /
// retry_after=300 next to a status that says nothing was measured.
//
// Not parallel: it arms process-global env gates via t.Setenv.
func TestQuotaHealthBlindStampClearsFailureObservation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// Phase 1 arms the health-blind marker; the container-liveness sub-gate makes
	// the anti-corr gate block this (ever-bound, stale-heartbeat) account, which is
	// the only route into the health-blind branch of the poller.
	t.Setenv(FarmLivenessDetectionEnvVar, "true")
	t.Setenv(coreauth.FarmRequireContainerAliveEnvVar, "1")

	handler, manager, exec := newLivenessQuotaTestHandler(t)
	registerClaudeAuth(t, manager, "claude-health-blind", true)

	auth, ok := manager.GetByID("claude-health-blind")
	if !ok {
		t.Fatal("registered auth missing")
	}
	// Seed the stale observation of an earlier 429 alongside the failure status.
	seeded := auth.Clone()
	seeded.Metadata[quotaRefreshStatusMetadataKey] = quotaRefreshStatusError
	seeded.Metadata[quotaRefreshErrorMetadataKey] = "quota endpoint returned non-success status 429"
	seeded.Metadata[quotaRefreshHTTPStatusMetadataKey] = 429
	seeded.Metadata[quotaRefreshRetryAfterMetadataKey] = int64(300)
	if _, err := manager.Update(context.Background(), seeded); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	current, _ := manager.GetByID("claude-health-blind")
	if got := metadataInt(current.Metadata, quotaRefreshHTTPStatusMetadataKey); got != 429 {
		t.Fatalf("precondition: seeded http_status = %d, want 429", got)
	}
	if !coreauth.RequireProvisionedBlocked(current) {
		t.Fatal("precondition: the anti-corr gate must be blocking this account from the normal poller")
	}
	if !coreauth.FarmHealthBlind(current) {
		t.Fatal("precondition: an ever-bound, gate-blocked farm account must be health-blind")
	}

	handler.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotTestPolicy(), false)

	// Leak boundary unchanged: the gate still blocks the probe, the stamp is a
	// pure metadata write.
	if got := exec.Calls(); got != 0 {
		t.Fatalf("HttpRequest calls = %d, want 0 (a gate-blocked account must never be probed)", got)
	}
	updated, ok := manager.GetByID("claude-health-blind")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusHealthBlind {
		t.Fatalf("quota status = %q, want %q", got, quotaRefreshStatusHealthBlind)
	}
	for _, key := range []string{quotaRefreshHTTPStatusMetadataKey, quotaRefreshRetryAfterMetadataKey} {
		if _, present := updated.Metadata[key]; present {
			t.Fatalf("metadata[%q] survived the health-blind stamp: %#v", key, updated.Metadata[key])
		}
	}
}

// TestQuotaRefreshLogAllowedThrottlesRepeatsAndPassesStateChanges pins the
// throttle contract behind the newly visible background failure / sticky-skip
// logs: repeats within the interval are suppressed, a changed signature is
// always emitted, and different accounts do not share a budget.
func TestQuotaRefreshLogAllowedThrottlesRepeatsAndPassesStateChanges(t *testing.T) {
	t.Parallel()

	handler := NewHandlerWithoutConfigFilePath(nil, coreauth.NewManager(nil, nil, nil))
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	const interval = 15 * time.Minute

	if !handler.quotaRefreshLogAllowed("failed", "auth-a", "http_status|429|", now, interval) {
		t.Fatal("first log for an account must be emitted")
	}
	if handler.quotaRefreshLogAllowed("failed", "auth-a", "http_status|429|", now.Add(time.Second), interval) {
		t.Fatal("identical signature within the interval must be throttled")
	}
	if !handler.quotaRefreshLogAllowed("failed", "auth-b", "http_status|429|", now.Add(time.Second), interval) {
		t.Fatal("a different account must not share the throttle budget")
	}
	changedAt := now.Add(2 * time.Second)
	if !handler.quotaRefreshLogAllowed("failed", "auth-a", "timeout|0|", changedAt, interval) {
		t.Fatal("a changed signature must be emitted immediately")
	}
	// The interval is measured from the last EMITTED line (changedAt), not from
	// the first call, so the repeat has to clear changedAt+interval.
	if handler.quotaRefreshLogAllowed("failed", "auth-a", "timeout|0|", changedAt.Add(interval-time.Second), interval) {
		t.Fatal("a repeat just under the interval must still be throttled")
	}
	if !handler.quotaRefreshLogAllowed("failed", "auth-a", "timeout|0|", changedAt.Add(interval+time.Second), interval) {
		t.Fatal("a repeat past the interval must be emitted again")
	}
	if !handler.quotaRefreshLogAllowed("sticky_skip", "auth-a", "timeout|0|", now.Add(2*time.Second), interval) {
		t.Fatal("a different event must not share the throttle budget")
	}
}

// TestQuotaBackgroundRefreshFailureIsActuallyLogged asserts the Warn line is
// really emitted through the real background-poller path. Every other test here
// pins persisted metadata or the HTTP response, so a refactor that silently
// dropped the log call (back to the invisible Debug this change exists to fix)
// would leave the whole suite green — exactly the regression class in scope.
//
// Not parallel: the logrus test hook is attached to the process-global standard
// logger.
func TestQuotaBackgroundRefreshFailureIsActuallyLogged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	hook := test.NewLocal(log.StandardLogger())
	t.Cleanup(func() {
		log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	})

	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{
		provider: "claude",
		responses: map[string]quotaSnapshotTestResponse{
			"https://api.anthropic.com/api/oauth/profile": {
				statusCode: http.StatusTooManyRequests,
				header:     http.Header{"Retry-After": []string{"300"}},
				body:       `{"error":{"type":"rate_limit_error","message":"slow down"}}`,
			},
		},
	}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ProxyURL: "http://test-proxy:8080",
		ID:       "claude-logged-429",
		Provider: "claude",
		Metadata: map[string]any{
			// Due in the past so the poller actually probes on this pass instead of
			// only installing an initial schedule.
			quotaNextRefreshMetadataKey: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)

	handler.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotTestPolicy(), false)

	var found *log.Entry
	for _, entry := range hook.AllEntries() {
		if entry.Data["event"] == "quota_background_refresh_failed" && entry.Data["auth_id"] == "claude-logged-429" {
			found = entry
			break
		}
	}
	if found == nil {
		t.Fatalf("no quota_background_refresh_failed entry emitted; captured %d entries", len(hook.AllEntries()))
	}
	// Warn specifically: the production log level is Info, so Debug/Trace would
	// make the failure invisible again.
	if found.Level != log.WarnLevel {
		t.Fatalf("log level = %v, want Warn (Info-level production logs must show it)", found.Level)
	}
	if got := found.Data["http_status"]; got != http.StatusTooManyRequests {
		t.Fatalf("log http_status = %#v, want 429", got)
	}
	if got := found.Data["retry_after_seconds"]; got != int64(300) {
		t.Fatalf("log retry_after_seconds = %#v, want 300", got)
	}
	if got := found.Data["error_class"]; got != "http_status" {
		t.Fatalf("log error_class = %#v, want http_status", got)
	}
	if got := found.Data["provider_error_type"]; got != "rate_limit_error" {
		t.Fatalf("log provider_error_type = %#v, want rate_limit_error", got)
	}
}

// TestQuotaStickySkipIsLoggedWithoutChangingTheSkipDecision asserts BOTH halves
// of its name: the sticky skip still skips (no upstream call, no schedule
// advance, status untouched) AND the Warn line is really emitted. The log half
// needs its own assertion for the same reason as
// TestQuotaBackgroundRefreshFailureIsActuallyLogged: every other signal of this
// branch is a non-event (nothing is called, nothing is written), so dropping
// logQuotaStickySkip would otherwise leave the suite green and put the frozen
// account back to being externally invisible.
//
// Not parallel: the logrus test hook is attached to the process-global standard
// logger.
func TestQuotaStickySkipIsLoggedWithoutChangingTheSkipDecision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	hook := test.NewLocal(log.StandardLogger())
	t.Cleanup(func() {
		log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
	})

	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "claude"}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ProxyURL: "http://test-proxy:8080",
		ID:       "claude-frozen",
		Provider: "claude",
		Status:   coreauth.StatusError,
		Metadata: map[string]any{
			quotaRefreshStatusMetadataKey: quotaRefreshStatusReauthRequired,
			quotaRefreshErrorMetadataKey:  claudeQuotaCredentialUnauthorizedMessage,
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)

	handler.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotTestPolicy(), false)

	if got := exec.Calls(); got != 0 {
		t.Fatalf("HttpRequest calls = %d, want 0 (sticky skip must not probe)", got)
	}
	updated, ok := manager.GetByID("claude-frozen")
	if !ok {
		t.Fatal("auth missing")
	}
	if _, hasNext := quotaSnapshotNextRefresh(updated); hasNext {
		t.Fatal("sticky skip must not advance quota_next_refresh_after")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusReauthRequired {
		t.Fatalf("persisted quota status = %q, want unchanged %q", got, quotaRefreshStatusReauthRequired)
	}

	var found *log.Entry
	for _, entry := range hook.AllEntries() {
		if entry.Data["event"] == "quota_background_refresh_sticky_skip" && entry.Data["auth_id"] == "claude-frozen" {
			found = entry
			break
		}
	}
	if found == nil {
		t.Fatalf("no quota_background_refresh_sticky_skip entry emitted; captured %d entries", len(hook.AllEntries()))
	}
	// Warn specifically: the production log level is Info, so Debug/Trace would
	// make the frozen account invisible again.
	if found.Level != log.WarnLevel {
		t.Fatalf("log level = %v, want Warn (Info-level production logs must show it)", found.Level)
	}
	if got := found.Data["quota_status"]; got != quotaRefreshStatusReauthRequired {
		t.Fatalf("log quota_status = %#v, want %q", got, quotaRefreshStatusReauthRequired)
	}

	// The skip log is throttled per account, so a second pass in the same window
	// must remain a silent no-op skip.
	before := len(hook.AllEntries())
	handler.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotTestPolicy(), false)
	if got := exec.Calls(); got != 0 {
		t.Fatalf("HttpRequest calls after a second pass = %d, want 0", got)
	}
	if got := len(hook.AllEntries()); got != before {
		t.Fatalf("entries after a throttled second pass = %d, want unchanged %d", got, before)
	}
}
