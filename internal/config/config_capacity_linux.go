package config

import (
	"os"

	"golang.org/x/sys/unix"
)

// Keep the locked descriptor and check predictable failures before changing bytes.
func preflightConfigReplacement(file *os.File, previousSize, nextSize int64) error {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
		return &os.PathError{Op: "get file size limit", Path: file.Name(), Err: err}
	}
	if uint64(nextSize) > limit.Cur {
		return &os.PathError{Op: "write config", Path: file.Name(), Err: unix.EFBIG}
	}
	if growth := nextSize - previousSize; growth > 0 {
		switch err := unix.Fallocate(int(file.Fd()), unix.FALLOC_FL_KEEP_SIZE, previousSize, growth); err {
		case nil, unix.EOPNOTSUPP, unix.ENOSYS, unix.EINVAL:
		default:
			return &os.PathError{Op: "reserve config space", Path: file.Name(), Err: err}
		}
	}
	return nil
}
