package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

func writeConfigYAML(path string, data []byte) error {
	// The legacy writer followed symlinks. Replace the resolved file instead
	// of replacing the link itself.
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	original, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer original.Close()
	info, err := original.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return &os.PathError{Op: "write config", Path: target, Err: unix.EINVAL}
	}
	parent := filepath.Dir(target)

	// A positive mount-ID boundary at an existing regular file identifies
	// a file mount, even if its parent is read-only. Do not fall back for
	// generic rename failures such as permissions or a read-only filesystem.
	if info.Sys().(*syscall.Stat_t).Nlink > 1 || isConfigFileMount(original, parent) {
		return writeConfigYAMLInPlace(target, info, data)
	}
	return replaceConfigYAML(target, parent, info, data, (*os.File).Sync)
}

func isConfigFileMount(original *os.File, parent string) bool {
	var fileInfo, parentInfo unix.Statx_t
	mask := unix.STATX_MNT_ID
	return unix.Statx(int(original.Fd()), "", unix.AT_EMPTY_PATH, mask, &fileInfo) == nil &&
		unix.Statx(unix.AT_FDCWD, parent, 0, mask, &parentInfo) == nil &&
		fileInfo.Mask&uint32(mask) != 0 && parentInfo.Mask&uint32(mask) != 0 &&
		fileInfo.Mnt_id != parentInfo.Mnt_id
}

func replaceConfigYAML(target, parent string, original os.FileInfo, data []byte, syncDirectory func(*os.File) error) error {
	dir, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	tmp, err := os.CreateTemp(parent, ".config-save-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	want := original.Sys().(*syscall.Stat_t)
	current, err := tmp.Stat()
	if err != nil {
		return err
	}
	got := current.Sys().(*syscall.Stat_t)
	if got.Uid != want.Uid || got.Gid != want.Gid {
		if err := tmp.Chown(int(want.Uid), int(want.Gid)); err != nil {
			return err
		}
	}
	if err := writeConfigBytes(tmp, data); err != nil {
		return err
	}
	if err := tmp.Chmod(original.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	current, err = os.Stat(target)
	if err != nil {
		return err
	}
	if !os.SameFile(current, original) || current.Sys().(*syscall.Stat_t).Nlink > 1 {
		return fmt.Errorf("config file or its hard links changed during save: %s", target)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return err
	}
	// Rename committed the visible file. A later directory sync error is
	// a durability warning; returning a failed save would disagree with disk.
	if err := syncDirectory(dir); err != nil {
		log.WithError(err).Warn("config replaced but syncing its directory failed")
	}
	return nil
}

func writeConfigBytes(file *os.File, data []byte) error {
	n, err := file.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func writeConfigYAMLInPlace(target string, original os.FileInfo, data []byte) error {
	file, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	current, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(current, original) {
		return fmt.Errorf("configuration changed during save: %s", target)
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
		return err
	}
	if uint64(len(data)) > limit.Cur {
		return &os.PathError{Op: "write config", Path: target, Err: unix.EFBIG}
	}
	before, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	// A private backup can be created even when the mount's parent is
	// read-only. It survives an unsuccessful restoration while this
	// process's temporary storage remains available.
	backup, err := os.CreateTemp("", "cliproxy-config-backup-*")
	if err != nil {
		return err
	}
	removeBackup := true
	defer func() {
		if removeBackup {
			_ = os.Remove(backup.Name())
		}
	}()
	backupErr := writeConfigBytes(backup, before)
	if backupErr == nil {
		backupErr = backup.Sync()
	}
	if err := errors.Join(backupErr, backup.Close()); err != nil {
		return fmt.Errorf("prepare configuration backup: %w", err)
	}
	if err := writeConfigWithRestore(file, before, data, file.WriteAt); err != nil {
		var recovery *configRestoreError
		if errors.As(err, &recovery) {
			removeBackup = false
			return fmt.Errorf("configuration restoration failed; previous bytes in %s: %w", backup.Name(), err)
		}
		return err
	}
	return nil
}

type configRestoreError struct{ err error }

func (e *configRestoreError) Error() string { return e.err.Error() }
func (e *configRestoreError) Unwrap() error { return e.err }

func writeConfigWithRestore(file *os.File, before, data []byte, writeAt func([]byte, int64) (int, error)) error {
	write := func(payload []byte, at func([]byte, int64) (int, error)) error {
		n, err := at(payload, 0)
		if err != nil {
			return err
		}
		if n != len(payload) {
			return io.ErrShortWrite
		}
		if err := file.Truncate(int64(len(payload))); err != nil {
			return err
		}
		return file.Sync()
	}
	err := write(data, writeAt)
	if err == nil {
		return nil
	}
	if restoreErr := write(before, file.WriteAt); restoreErr != nil {
		return &configRestoreError{errors.Join(err, fmt.Errorf("restore previous YAML: %w", restoreErr))}
	}
	return err
}
