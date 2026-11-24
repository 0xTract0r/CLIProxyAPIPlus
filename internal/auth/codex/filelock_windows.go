//go:build windows

package codex

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

const lockfileExclusiveLock = 0x00000002

func acquireInterProcessLockFile(authFilePath string) (func(), error) {
	lockPath := canonicalLockPath(authFilePath) + ".lock"
	return lockOpenedFile(os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600))
}

func lockOpenedFile(f *os.File, errOpen error) (func(), error) {
	if errOpen != nil {
		return nil, fmt.Errorf("failed to open credential lock file: %w", errOpen)
	}
	var overlapped syscall.Overlapped
	r1, _, errLock := procLockFileEx.Call(
		f.Fd(),
		uintptr(lockfileExclusiveLock),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if r1 == 0 {
		_ = f.Close()
		return nil, fmt.Errorf("failed to lock credential file %s: %w", f.Name(), errLock)
	}
	return func() {
		var ov syscall.Overlapped
		_, _, _ = procUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ov)))
		_ = f.Close()
	}, nil
}
