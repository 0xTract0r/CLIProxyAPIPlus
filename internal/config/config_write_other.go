//go:build !linux

package config

import "os"

func writeConfigYAML(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
