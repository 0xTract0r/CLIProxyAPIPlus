package management

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestClaims333PriorIdentityVeto(t *testing.T) {
	jwt := func(email string) string {
		b, _ := json.Marshal(map[string]any{"email": email, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "account"}})
		return "e30." + base64.RawURLEncoding.EncodeToString(b) + "."
	}
	for _, source := range []string{"metadata", "id-token"} {
		for _, location := range []string{"access_token", "id_token", "siwc-access", "siwc-id"} {
			t.Run(source+"/"+location, func(t *testing.T) {
				prior := map[string]any{"account_id": "account"}
				if source == "metadata" {
					prior["email"] = "old@example.invalid"
				} else {
					prior["id_token"] = jwt("old@example.invalid")
				}
				incoming := map[string]any{"account_id": "account"}
				key := location
				target := incoming
				if location == "siwc-access" || location == "siwc-id" {
					target = map[string]any{}
					incoming["siwc"] = target
					if location == "siwc-access" {
						key = "access_token"
					} else {
						key = "id_token"
					}
				}
				target[key] = jwt("other@example.invalid")
				if codexRecoveryTokenClaimsConsistent(incoming, prior) {
					t.Fatal("conflicting token email bypassed prior identity")
				}
				target[key] = jwt("OLD@example.invalid")
				if !codexRecoveryTokenClaimsConsistent(incoming, prior) {
					t.Fatal("same identity rejected")
				}
				if _, exists := incoming["email"]; exists {
					t.Fatal("prior identity leaked into submitted fields")
				}
			})
		}
	}
}
