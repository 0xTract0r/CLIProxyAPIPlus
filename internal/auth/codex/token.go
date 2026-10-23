// Package codex exposes OAuth clients and backward-compatible credential storage.
package codex

import "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex/credential"

type CodexTokenStorage = credential.CodexTokenStorage

func WithFileLock(path string, fn func() error) error { return credential.WithFileLock(path, fn) }
func WriteJSONFileAtomic(path string, data map[string]any) error {
	return credential.WriteJSONFileAtomic(path, data)
}
func MergeWithDiskMap(disk, incoming map[string]any) map[string]any {
	return credential.MergeWithDiskMap(disk, incoming)
}
func MergeCodexCredentialsFromDisk(disk, incoming map[string]any) map[string]any {
	return credential.MergeCodexCredentialsFromDisk(disk, incoming)
}
