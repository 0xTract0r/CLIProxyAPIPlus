package helps

import (
	"bytes"
	"regexp"
	"testing"

	"github.com/tidwall/gjson"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// newTestSalt persists a deterministic salt under a temp dir so derivation is
// stable and assertions do not depend on a randomly generated process salt.
func newTestAuthDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func TestSyntheticDeviceID_DiffersBetweenAccounts(t *testing.T) {
	dir := newTestAuthDir(t)
	authA := &cliproxyauth.Auth{FileName: "account-a.json"}
	authB := &cliproxyauth.Auth{FileName: "account-b.json"}

	idA := SyntheticDeviceID(dir, authA, "")
	idB := SyntheticDeviceID(dir, authB, "")

	if idA == idB {
		t.Fatalf("expected distinct device ids for different accounts, got %q for both", idA)
	}
	if !hex64.MatchString(idA) || !hex64.MatchString(idB) {
		t.Fatalf("expected 64-hex device ids, got %q and %q", idA, idB)
	}
}

func TestSyntheticDeviceID_StableAcrossAPIKeys(t *testing.T) {
	dir := newTestAuthDir(t)
	auth := &cliproxyauth.Auth{FileName: "account-a.json"}

	first := SyntheticDeviceID(dir, auth, "api-key-1")
	second := SyntheticDeviceID(dir, auth, "api-key-2")
	third := SyntheticDeviceID(dir, auth, "")

	if first != second || first != third {
		t.Fatalf("expected device id stable across apiKeys, got %q, %q, %q", first, second, third)
	}
}

func TestSyntheticDeviceID_StableAcrossSaltReload(t *testing.T) {
	dir := newTestAuthDir(t)
	auth := &cliproxyauth.Auth{ID: "auth-123"}

	first := SyntheticDeviceID(dir, auth, "")

	// Force a salt reload by clearing the in-memory cache; the persisted salt file
	// must yield the same derivation (simulating a process restart).
	syntheticDeviceSaltMu.Lock()
	syntheticDeviceSaltVal = nil
	syntheticDeviceSaltDir = ""
	syntheticDeviceSaltMu.Unlock()

	second := SyntheticDeviceID(dir, auth, "")
	if first != second {
		t.Fatalf("expected device id stable across salt reload, got %q then %q", first, second)
	}
}

func TestInjectAccountDeviceID_ReplacesOnlyDeviceID(t *testing.T) {
	dir := newTestAuthDir(t)
	auth := &cliproxyauth.Auth{FileName: "account-a.json"}

	payload := []byte(`{"model":"claude","metadata":{"user_id":{"device_id":"realdevice","account_uuid":"acct-uuid","session_id":"sess-uuid"}},"messages":[{"role":"user","content":"hi"}]}`)

	out := InjectAccountDeviceID(payload, dir, auth, "api-key-1")

	gotDevice := gjson.GetBytes(out, "metadata.user_id.device_id").String()
	if gotDevice == "realdevice" {
		t.Fatalf("expected device_id to be rewritten, still got real value")
	}
	if !hex64.MatchString(gotDevice) {
		t.Fatalf("expected synthetic 64-hex device_id, got %q", gotDevice)
	}
	// session_id and account_uuid must be preserved untouched.
	if got := gjson.GetBytes(out, "metadata.user_id.session_id").String(); got != "sess-uuid" {
		t.Fatalf("expected session_id preserved, got %q", got)
	}
	if got := gjson.GetBytes(out, "metadata.user_id.account_uuid").String(); got != "acct-uuid" {
		t.Fatalf("expected account_uuid preserved, got %q", got)
	}
	// Unrelated fields (messages) must not be touched (cache integrity).
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hi" {
		t.Fatalf("expected messages untouched, got %q", got)
	}
}

func TestInjectAccountDeviceID_BuildsObjectWhenMissing(t *testing.T) {
	dir := newTestAuthDir(t)
	auth := &cliproxyauth.Auth{FileName: "account-a.json"}

	payload := []byte(`{"model":"claude","messages":[]}`)
	out := InjectAccountDeviceID(payload, dir, auth, "")

	device := gjson.GetBytes(out, "metadata.user_id.device_id").String()
	if !hex64.MatchString(device) {
		t.Fatalf("expected synthetic device_id object built, got %q", device)
	}
	if !gjson.GetBytes(out, "metadata.user_id.session_id").Exists() {
		t.Fatalf("expected a session_id to be generated")
	}
}

func TestInjectAccountDeviceID_ReplacesLegacyFlatString(t *testing.T) {
	dir := newTestAuthDir(t)
	auth := &cliproxyauth.Auth{FileName: "account-a.json"}

	payload := []byte(`{"metadata":{"user_id":"user_abc_account_x_session_y"}}`)
	out := InjectAccountDeviceID(payload, dir, auth, "")

	if !gjson.GetBytes(out, "metadata.user_id").IsObject() {
		t.Fatalf("expected legacy flat string replaced with synthetic object")
	}
	device := gjson.GetBytes(out, "metadata.user_id.device_id").String()
	if !hex64.MatchString(device) {
		t.Fatalf("expected synthetic device_id, got %q", device)
	}
}

// TestInjectAccountDeviceID_InvalidPayloadPassesThrough covers P1.4: when the body
// cannot be parsed, the helper must not error or rewrite anything; it returns the
// original bytes so the request is forwarded verbatim (never a 400).
func TestInjectAccountDeviceID_InvalidPayloadPassesThrough(t *testing.T) {
	dir := newTestAuthDir(t)
	auth := &cliproxyauth.Auth{FileName: "account-a.json"}

	invalid := []byte(`{"metadata": this is not json`)
	out := InjectAccountDeviceID(invalid, dir, auth, "")

	if !bytes.Equal(out, invalid) {
		t.Fatalf("expected invalid payload returned unchanged, got %q", out)
	}
}

func TestInjectAccountDeviceID_EmptyPayloadPassesThrough(t *testing.T) {
	dir := newTestAuthDir(t)
	auth := &cliproxyauth.Auth{FileName: "account-a.json"}

	out := InjectAccountDeviceID(nil, dir, auth, "")
	if len(out) != 0 {
		t.Fatalf("expected empty payload returned unchanged, got %q", out)
	}
}

// TestInjectAccountDeviceIDWithOptions_NoFabricate_ReplacesExistingDeviceID covers
// the count_tokens path with fabricateIfMissing=false: when metadata.user_id is a
// JSON object, device_id is still swapped to the account-derived value while
// session_id and account_uuid stay untouched.
func TestInjectAccountDeviceIDWithOptions_NoFabricate_ReplacesExistingDeviceID(t *testing.T) {
	dir := newTestAuthDir(t)
	auth := &cliproxyauth.Auth{FileName: "account-a.json"}

	payload := []byte(`{"model":"claude","metadata":{"user_id":{"device_id":"realdevice","account_uuid":"acct-uuid","session_id":"sess-uuid"}},"messages":[{"role":"user","content":"hi"}]}`)

	out := InjectAccountDeviceIDWithOptions(payload, dir, auth, "api-key-1", false)

	gotDevice := gjson.GetBytes(out, "metadata.user_id.device_id").String()
	if gotDevice == "realdevice" {
		t.Fatalf("expected device_id to be rewritten, still got real value")
	}
	if !hex64.MatchString(gotDevice) {
		t.Fatalf("expected synthetic 64-hex device_id, got %q", gotDevice)
	}
	// It must match the value produced by the Execute/main path for the same account.
	wantDevice := SyntheticDeviceID(dir, auth, "api-key-1")
	if gotDevice != wantDevice {
		t.Fatalf("expected device_id derived from the same account scope, got %q want %q", gotDevice, wantDevice)
	}
	if got := gjson.GetBytes(out, "metadata.user_id.session_id").String(); got != "sess-uuid" {
		t.Fatalf("expected session_id preserved, got %q", got)
	}
	if got := gjson.GetBytes(out, "metadata.user_id.account_uuid").String(); got != "acct-uuid" {
		t.Fatalf("expected account_uuid preserved, got %q", got)
	}
}

// TestInjectAccountDeviceIDWithOptions_NoFabricate_LeavesMissingUntouched covers the
// count_tokens safety rule: when metadata.user_id is absent and fabrication is
// disabled, the body is returned verbatim so we never emit a field the real client
// did not send.
func TestInjectAccountDeviceIDWithOptions_NoFabricate_LeavesMissingUntouched(t *testing.T) {
	dir := newTestAuthDir(t)
	auth := &cliproxyauth.Auth{FileName: "account-a.json"}

	payload := []byte(`{"model":"claude","messages":[]}`)
	out := InjectAccountDeviceIDWithOptions(payload, dir, auth, "", false)

	if !bytes.Equal(out, payload) {
		t.Fatalf("expected missing metadata.user_id left untouched, got %q", out)
	}
	if gjson.GetBytes(out, "metadata.user_id").Exists() {
		t.Fatalf("expected no metadata.user_id fabricated, got %q", out)
	}
}

// TestInjectAccountDeviceIDWithOptions_NoFabricate_MetadataPresentNoUserID confirms
// that an existing metadata object without user_id is not augmented when fabrication
// is disabled.
func TestInjectAccountDeviceIDWithOptions_NoFabricate_MetadataPresentNoUserID(t *testing.T) {
	dir := newTestAuthDir(t)
	auth := &cliproxyauth.Auth{FileName: "account-a.json"}

	payload := []byte(`{"model":"claude","metadata":{"foo":"bar"},"messages":[]}`)
	out := InjectAccountDeviceIDWithOptions(payload, dir, auth, "", false)

	if !bytes.Equal(out, payload) {
		t.Fatalf("expected metadata without user_id left untouched, got %q", out)
	}
	if gjson.GetBytes(out, "metadata.user_id").Exists() {
		t.Fatalf("expected no metadata.user_id fabricated, got %q", out)
	}
}

// TestInjectAccountDeviceIDWithOptions_NoFabricate_ReplacesLegacyFlatString confirms
// that a present-but-flat user_id is still normalized to a synthetic object even
// with fabrication disabled: the no-fabricate rule only applies to a truly absent
// field, matching the leader's spec ("非 JSON 扁平串 → 整体换合成 JSON").
func TestInjectAccountDeviceIDWithOptions_NoFabricate_ReplacesLegacyFlatString(t *testing.T) {
	dir := newTestAuthDir(t)
	auth := &cliproxyauth.Auth{FileName: "account-a.json"}

	payload := []byte(`{"metadata":{"user_id":"user_abc_account_x_session_y"}}`)
	out := InjectAccountDeviceIDWithOptions(payload, dir, auth, "", false)

	if !gjson.GetBytes(out, "metadata.user_id").IsObject() {
		t.Fatalf("expected legacy flat string replaced with synthetic object")
	}
	device := gjson.GetBytes(out, "metadata.user_id.device_id").String()
	if !hex64.MatchString(device) {
		t.Fatalf("expected synthetic device_id, got %q", device)
	}
}

// TestInjectAccountDeviceIDWithOptions_NoFabricate_InvalidPayloadPassesThrough
// confirms the parse-failure safe pass-through still holds on the count_tokens path.
func TestInjectAccountDeviceIDWithOptions_NoFabricate_InvalidPayloadPassesThrough(t *testing.T) {
	dir := newTestAuthDir(t)
	auth := &cliproxyauth.Auth{FileName: "account-a.json"}

	invalid := []byte(`{"metadata": this is not json`)
	out := InjectAccountDeviceIDWithOptions(invalid, dir, auth, "", false)

	if !bytes.Equal(out, invalid) {
		t.Fatalf("expected invalid payload returned unchanged, got %q", out)
	}
}

// TestInjectAccountDeviceID_MainPathStillFabricates guards against a regression on
// the main messages path: the fabricate-default wrapper must keep creating a
// synthetic metadata.user_id when the field is missing.
func TestInjectAccountDeviceID_MainPathStillFabricates(t *testing.T) {
	dir := newTestAuthDir(t)
	auth := &cliproxyauth.Auth{FileName: "account-a.json"}

	payload := []byte(`{"model":"claude","messages":[]}`)
	out := InjectAccountDeviceID(payload, dir, auth, "")

	if !gjson.GetBytes(out, "metadata.user_id").IsObject() {
		t.Fatalf("expected main path to fabricate metadata.user_id, got %q", out)
	}
	device := gjson.GetBytes(out, "metadata.user_id.device_id").String()
	if !hex64.MatchString(device) {
		t.Fatalf("expected synthetic device_id, got %q", device)
	}
}

func TestSyntheticDeviceID_FallsBackToProcessSaltWithoutAuthDir(t *testing.T) {
	auth := &cliproxyauth.Auth{FileName: "account-a.json"}

	first := SyntheticDeviceID("", auth, "")
	second := SyntheticDeviceID("", auth, "")
	if first != second {
		t.Fatalf("expected stable device id from process salt, got %q then %q", first, second)
	}
	if !hex64.MatchString(first) {
		t.Fatalf("expected 64-hex device id, got %q", first)
	}
}
