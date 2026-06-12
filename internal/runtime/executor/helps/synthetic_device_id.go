package helps

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// syntheticDeviceSaltFileName is the file under the auth directory that holds the
// per-server random salt used to derive synthetic device IDs. The salt makes the
// derivation deterministic per server while preventing anyone from reversing a
// synthetic device_id back to the real account scope key.
const syntheticDeviceSaltFileName = ".synthetic-device-salt"

var (
	syntheticDeviceSaltMu   sync.Mutex
	syntheticDeviceSaltVal  []byte
	syntheticDeviceSaltDir  string
	syntheticProcessSalt    []byte
	syntheticProcessSaltSet sync.Once
)

// serverSyntheticDeviceSalt returns a stable per-server secret salt.
//
// Resolution order:
//  1. A persisted random salt file under authDir (created on first use). This keeps
//     the salt stable across process restarts so a given account always derives the
//     same synthetic device_id.
//  2. A process-lifetime random salt when authDir is empty or not writable. This is
//     still stable within a process, so device IDs stay consistent across requests;
//     it only changes if the server restarts without a persisted salt.
//
// The returned salt is never exposed to clients or upstream; only its SHA-256
// derivation with the account scope key leaves the process.
func serverSyntheticDeviceSalt(authDir string) []byte {
	authDir = strings.TrimSpace(authDir)
	if authDir != "" {
		syntheticDeviceSaltMu.Lock()
		defer syntheticDeviceSaltMu.Unlock()
		if syntheticDeviceSaltVal != nil && syntheticDeviceSaltDir == authDir {
			return syntheticDeviceSaltVal
		}
		if salt, ok := loadOrCreateSyntheticDeviceSalt(authDir); ok {
			syntheticDeviceSaltVal = salt
			syntheticDeviceSaltDir = authDir
			return salt
		}
	}
	return processSyntheticDeviceSalt()
}

func processSyntheticDeviceSalt() []byte {
	syntheticProcessSaltSet.Do(func() {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			// rand.Read should not fail; fall back to a fixed sentinel so the
			// process keeps working deterministically rather than panicking.
			for i := range buf {
				buf[i] = 0x5a
			}
		}
		syntheticProcessSalt = buf
	})
	return syntheticProcessSalt
}

// loadOrCreateSyntheticDeviceSalt reads the persisted salt, creating it on first
// use. It returns false when the salt cannot be read or written, so the caller can
// fall back to a process-lifetime salt.
func loadOrCreateSyntheticDeviceSalt(authDir string) ([]byte, bool) {
	path := filepath.Join(authDir, syntheticDeviceSaltFileName)
	if data, err := os.ReadFile(path); err == nil {
		decoded := decodeSyntheticDeviceSalt(data)
		if len(decoded) >= 16 {
			return decoded, true
		}
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, false
	}
	encoded := []byte(hex.EncodeToString(buf) + "\n")
	// Best-effort persistence; if the directory is read-only the process salt is used.
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return nil, false
	}
	return buf, true
}

func decodeSyntheticDeviceSalt(data []byte) []byte {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}
	if decoded, err := hex.DecodeString(trimmed); err == nil && len(decoded) > 0 {
		return decoded
	}
	// Tolerate a raw (non-hex) salt file as opaque bytes.
	return []byte(trimmed)
}

// SyntheticDeviceID derives a per-account synthetic device_id from the server salt
// and the account scope key (shared with the device profile cache via
// ClaudeAccountScopeKey). The result is a 64-char hex string that is:
//   - stable for a given upstream account across requests, apiKeys and restarts
//     (when the salt is persisted),
//   - different between distinct upstream accounts,
//   - opaque and free of PII, and not reversible to the real device.
func SyntheticDeviceID(authDir string, auth *cliproxyauth.Auth, apiKey string) string {
	salt := serverSyntheticDeviceSalt(authDir)
	scopeKey := ClaudeAccountScopeKey(auth, apiKey)
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte("\x00"))
	h.Write([]byte(scopeKey))
	return hex.EncodeToString(h.Sum(nil))
}

// InjectAccountDeviceID rewrites only the device_id inside metadata.user_id with a
// per-account synthetic value, while preserving the JSON shape Claude Code sends:
//
//	metadata.user_id = {"device_id":"<64hex>","account_uuid":"","session_id":"<uuid>"}
//
// Behavior:
//   - metadata.user_id is a JSON object: replace device_id only; keep account_uuid
//     and session_id (the client's per-session value) untouched.
//   - metadata.user_id is missing or not a JSON object (e.g. the legacy flat string):
//     replace it with a synthetic object carrying a fresh session_id.
//   - Any failure to mutate the payload is a safe no-op: the original payload is
//     returned unchanged so the request is never rejected with a 400. The caller
//     therefore always passes through valid bodies.
func InjectAccountDeviceID(payload []byte, authDir string, auth *cliproxyauth.Auth, apiKey string) []byte {
	deviceID := SyntheticDeviceID(authDir, auth, apiKey)

	if !gjson.ValidBytes(payload) {
		// Do not attempt to rewrite an unparseable body; pass it through.
		return payload
	}

	userID := gjson.GetBytes(payload, "metadata.user_id")
	if userID.Exists() && userID.IsObject() {
		updated, err := sjson.SetBytes(payload, "metadata.user_id.device_id", deviceID)
		if err != nil {
			return payload
		}
		return updated
	}

	// Missing metadata.user_id or a non-object value (legacy flat string): set a
	// synthetic object. account_uuid stays empty per the device-id design; the
	// session_id is regenerated since the prior value (if any) is not reusable here.
	synthetic := map[string]string{
		"device_id":    deviceID,
		"account_uuid": "",
		"session_id":   uuid.New().String(),
	}
	updated, err := sjson.SetBytes(payload, "metadata.user_id", synthetic)
	if err != nil {
		return payload
	}
	return updated
}
