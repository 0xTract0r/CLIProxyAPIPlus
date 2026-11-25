//go:build !windows && !aix && !solaris

package codex

import (
	"fmt"
	"os"
	"syscall"
)

func acquireInterProcessLockFile(authFilePath string) (func(), error) {
	lockPath := canonicalLockPath(authFilePath) + ".lock"
	return lockOpenedFile(os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600))
}

func lockOpenedFile(f *os.File, errOpen error) (func(), error) {
	if errOpen != nil {
		return nil, fmt.Errorf("failed to open credential lock file: %w", errOpen)
	}
	if errFlock := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); errFlock != nil {
		_ = f.Close()
		return nil, fmt.Errorf("failed to acquire flock on %s: %w", f.Name(), errFlock)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
