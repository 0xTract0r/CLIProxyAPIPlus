package management

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Fork (fix-oauth-egress-anticorr): a card re-auth (auth_name) writes the new
// tokens into the TARGET account's credential file under the target's proxy,
// managed-header seed and device identity. It must therefore only persist when
// the browser login was that same account; otherwise re-authenticating T while
// logged in as O would overwrite T's refresh token with O's credentials (Claude)
// or bind O's file to T's proxy and seed (Codex), linking the two accounts.

// maskEmailForLog keeps the first character of the local part and the domain
// so operators can correlate accounts without the full address in logs.
func maskEmailForLog(email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return "<empty>"
	}
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return email[:1] + "***"
	}
	return email[:1] + "***" + email[at:]
}

// claudeReauthTargetEmail returns the email of a Claude re-auth target: the
// persisted metadata email first, else the email encoded in the canonical
// claude-<email>.json file name. It returns "" when neither is available.
func claudeReauthTargetEmail(target *coreauth.Auth) string {
	if target == nil {
		return ""
	}
	if email := strings.TrimSpace(metadataString(target.Metadata, "email")); email != "" {
		return email
	}
	for _, name := range []string{target.FileName, target.ID, authAttribute(target, "path")} {
		name = filepath.Base(strings.TrimSpace(name))
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "claude-") || !strings.HasSuffix(lower, ".json") {
			continue
		}
		email := strings.TrimSpace(name[len("claude-") : len(name)-len(".json")])
		if strings.Contains(email, "@") {
			return email
		}
	}
	return ""
}

// verifyClaudeReauthIdentity fails closed unless the account that completed the
// browser login is the re-auth target (case-insensitive email match). A target
// whose email cannot be determined is rejected as unverifiable.
func verifyClaudeReauthIdentity(target *coreauth.Auth, gotEmail string) error {
	if target == nil {
		return nil
	}
	want := claudeReauthTargetEmail(target)
	if want == "" {
		return errors.New("re-auth target has no recorded email, so the logged-in account cannot be verified; credentials were not written")
	}
	got := strings.TrimSpace(gotEmail)
	if got == "" || !strings.EqualFold(got, want) {
		return fmt.Errorf("logged-in account %s does not match the re-auth target %s; credentials were not written", maskEmailForLog(got), maskEmailForLog(want))
	}
	return nil
}

// verifyCodexReauthIdentity fails closed unless the ChatGPT account that
// completed the browser login is the re-auth target: chatgpt_account_id must
// equal the target's account_id (a plan change under the same account_id is
// allowed), and the email must match as well when the target records one.
func verifyCodexReauthIdentity(target *coreauth.Auth, gotAccountID, gotEmail string) error {
	if target == nil {
		return nil
	}
	wantAccountID := strings.TrimSpace(metadataString(target.Metadata, "account_id"))
	if wantAccountID == "" {
		return errors.New("re-auth target has no recorded account_id, so the logged-in account cannot be verified; credentials were not written")
	}
	wantEmail := strings.TrimSpace(authEmail(target))
	gotAccountID = strings.TrimSpace(gotAccountID)
	gotEmail = strings.TrimSpace(gotEmail)
	if gotAccountID == "" || gotAccountID != wantAccountID {
		return fmt.Errorf("logged-in account %s does not match the re-auth target %s (account_id differs); credentials were not written", maskEmailForLog(gotEmail), maskEmailForLog(wantEmail))
	}
	if wantEmail != "" && !strings.EqualFold(gotEmail, wantEmail) {
		return fmt.Errorf("logged-in account %s does not match the re-auth target %s (email differs); credentials were not written", maskEmailForLog(gotEmail), maskEmailForLog(wantEmail))
	}
	return nil
}

// inheritReauthAccountSettings carries the re-auth target's persisted
// account_settings onto the new record so a card re-auth keeps operator
// settings (extra_headers, refresh_enabled, transport / tls profile) and the
// runtime identity state (including created_at) instead of regenerating them.
// The OAuth-page new-account path never calls this.
func inheritReauthAccountSettings(record, target *coreauth.Auth) {
	if record == nil || target == nil || target.Metadata == nil {
		return
	}
	settings, ok := target.Metadata["account_settings"]
	if !ok || settings == nil {
		return
	}
	if record.Metadata == nil {
		record.Metadata = make(map[string]any)
	}
	record.Metadata["account_settings"] = settings
}

// preserveReauthRuntimeIdentityCreatedAt restores the target's runtime identity
// created_at after account_settings were re-merged for the re-auth record. The
// merge rotates the runtime identity snapshot when its derived identity_id
// changes (e.g. a plan change renames the credential file), which would
// otherwise reset created_at and make an existing account look brand new.
func (h *Handler) preserveReauthRuntimeIdentityCreatedAt(record, target *coreauth.Auth) {
	if record == nil || target == nil || record.Metadata == nil {
		return
	}
	var cfg *config.Config
	if h != nil {
		cfg = h.cfg
	}
	previous := readAccountSettingsMetadata(target, cfg)
	if previous.RuntimeIdentityState == nil || previous.RuntimeIdentityState.Current == nil {
		return
	}
	createdAt := strings.TrimSpace(previous.RuntimeIdentityState.Current.CreatedAt)
	if createdAt == "" {
		return
	}
	stored := readAccountSettingsMetadata(record, cfg)
	if stored.RuntimeIdentityState == nil || stored.RuntimeIdentityState.Current == nil {
		return
	}
	stored.RuntimeIdentityState.Current.CreatedAt = createdAt
	record.Metadata["account_settings"] = stored
}
