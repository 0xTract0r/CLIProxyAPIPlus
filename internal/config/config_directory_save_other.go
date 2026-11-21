//go:build !linux

package config

import "os"

func (f *lockedConfigFile) replaceDirectoryFile([]byte) (*ConfigFileSnapshot, error) {
	return nil, nil
}

func configReadAttempts() int { return 1 }

func checkConfigWriteIntegrity(*os.File) error { return nil }
