//go:build !linux

package config

func (f *lockedConfigFile) replaceDirectoryFile([]byte) (*ConfigFileSnapshot, error) {
	return nil, nil
}

func configReadAttempts() int { return 1 }
