package codex

import "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex/credential"

type CredentialRefreshPatch = credential.CredentialRefreshPatch

func NewCredentialRefreshPatch(before, after map[string]any, outer, siwc bool) *CredentialRefreshPatch {
	return credential.NewCredentialRefreshPatch(before, after, outer, siwc)
}
func NewCredentialRefreshPatchWithDiskBaseline(before, diskBefore, after map[string]any, outer, siwc bool) *CredentialRefreshPatch {
	return credential.NewCredentialRefreshPatchWithDiskBaseline(before, diskBefore, after, outer, siwc)
}
func IsCodexCredentialKey(key string) bool { return credential.IsCodexCredentialKey(key) }
