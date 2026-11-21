//go:build !linux

package config

import "os"

func preflightConfigReplacement(_ *os.File, _, _ int64) error { return nil }
