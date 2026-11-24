//go:build aix || solaris

package codex

import (
	"fmt"
	"os"
	"syscall"

	"github.com/gofrs/flock"
	log "github.com/sirupsen/logrus"
)

func acquireInterProcessLockFile(authFilePath string) (func(), error) {
	lockPath := canonicalLockPath(authFilePath) + ".lock"
	lock := flock.New(lockPath)
	if errLock := lock.Lock(); errLock != nil {
		return nil, fmt.Errorf("failed to lock credential file %s: %w", lockPath, errLock)
	}
	return func() {
		if errClose := lock.Close(); errClose != nil {
			log.Errorf("codex token storage: release credential file lock: %v", errClose)
		}
	}, nil
}

// Lock the descriptor already opened beneath the credential root.
func lockOpenedFile(f *os.File, errOpen error) (func(), error) {
	if errOpen != nil {
		return nil, fmt.Errorf("failed to open credential lock file: %w", errOpen)
	}
	record := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if errLock := syscall.FcntlFlock(f.Fd(), syscall.F_SETLKW, &record); errLock != nil {
		_ = f.Close()
		return nil, fmt.Errorf("failed to lock credential file %s: %w", f.Name(), errLock)
	}
	return func() { _ = f.Close() }, nil
}
