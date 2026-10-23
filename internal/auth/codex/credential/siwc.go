package credential

// CodexSIWCTokenData holds the OAuth token information obtained from OpenAI's Sign in with ChatGPT (SIWC) flow.
type CodexSIWCTokenData struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	IDToken      string   `json:"id_token,omitempty"`
	ClientID     string   `json:"client_id"`
	Scope        string   `json:"scope,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
	Expire       string   `json:"expired"`
	ExpiresAt    string   `json:"expires_at,omitempty"`
	LastRefresh  string   `json:"last_refresh"`
	DeviceID     string   `json:"device_id,omitempty"`
}

// ToMap converts CodexSIWCTokenData into a map suitable for credential metadata persistence.
func (d *CodexSIWCTokenData) ToMap() map[string]any {
	if d == nil {
		return nil
	}
	out := map[string]any{
		"access_token":  d.AccessToken,
		"refresh_token": d.RefreshToken,
		"client_id":     d.ClientID,
		"expired":       d.Expire,
		"last_refresh":  d.LastRefresh,
	}
	if d.ExpiresAt != "" {
		out["expires_at"] = d.ExpiresAt
	}
	if d.IDToken != "" {
		out["id_token"] = d.IDToken
	}
	if d.Scope != "" {
		out["scope"] = d.Scope
	}
	if len(d.Scopes) > 0 {
		scopesCopy := make([]string, len(d.Scopes))
		copy(scopesCopy, d.Scopes)
		out["scopes"] = scopesCopy
	}
	if d.DeviceID != "" {
		out["device_id"] = d.DeviceID
	}
	return out
}
