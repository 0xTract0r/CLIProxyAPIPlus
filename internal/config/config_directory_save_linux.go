package config

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const linuxDirectorySaveEnv = "CPA_CONFIG_LINUX_ATOMIC_REPLACE"

func configReadAttempts() int {
	if os.Getenv(linuxDirectorySaveEnv) == "1" {
		return 3
	}
	return 1
}

// replaceDirectoryFile opts in only for a conservatively identified local file.
// A nil snapshot and error means the original in-place protocol is applicable.
// Once the replacement is being prepared, an error is returned to the caller.
func (f *lockedConfigFile) replaceDirectoryFile(data []byte) (*ConfigFileSnapshot, error) {
	if os.Getenv(linuxDirectorySaveEnv) != "1" || f.location.root != nil || lookupEphemeralServerID(f.snapshot.info) != "" {
		return nil, nil
	}
	path, err := filepath.Abs(f.location.path)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if resolved != path {
		return nil, nil
	}
	parent, base := filepath.Dir(path), filepath.Base(path)
	folder, err := os.OpenRoot(parent)
	if err != nil {
		if isConfigReadOnly(err) {
			return nil, nil
		}
		return nil, err
	}
	defer folder.Close()
	dir, err := folder.Open(".")
	if err != nil {
		if isConfigReadOnly(err) {
			return nil, nil
		}
		return nil, err
	}
	defer dir.Close()
	dirInfo, err := dir.Stat()
	if err != nil {
		return nil, err
	}
	dirID := dirInfo.Sys().(*syscall.Stat_t)
	old := f.snapshot.info.Sys().(*syscall.Stat_t)
	if dirID.Uid != uint32(os.Geteuid()) || dirInfo.Mode().Perm()&022 != 0 || old.Uid != dirID.Uid ||
		old.Nlink != 1 || f.snapshot.info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 ||
		!sameConfigMount(f.file, dir) || !knownLocalConfigFilesystem(dir) {
		return nil, nil
	}
	if err := unix.Faccessat(int(dir.Fd()), ".", unix.W_OK, unix.AT_EACCESS); err != nil {
		if isConfigReadOnly(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("check configuration directory write access: %w", err)
	}
	if attrs, err := configDirectoryHasXattrs(f.file); err != nil || attrs {
		return nil, err
	}
	// From here on every failure leaves the previously committed path in place.
	name := ".config-" + rand.Text()
	tmp, err := folder.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = folder.Remove(name)
		}
	}()
	if err := lockConfigFile(tmp, true); err != nil {
		return nil, fmt.Errorf("lock replacement config: %w", err)
	}
	if err := unix.Fremovexattr(int(tmp.Fd()), "system.posix_acl_access"); err != nil &&
		!errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.EOPNOTSUPP) {
		return nil, fmt.Errorf("remove inherited configuration ACL: %w", err)
	}
	if attrs, err := configDirectoryHasXattrs(tmp); err != nil || attrs {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unexpected replacement configuration metadata")
	}
	tempInfo, err := tmp.Stat()
	if err != nil {
		return nil, err
	}
	if got := tempInfo.Sys().(*syscall.Stat_t); got.Uid != old.Uid || got.Gid != old.Gid {
		if err := tmp.Chown(int(old.Uid), int(old.Gid)); err != nil {
			return nil, err
		}
	}
	if err := preflightConfigReplacement(tmp, 0, int64(len(data))); err != nil {
		return nil, err
	}
	if _, err := tmp.Write(data); err != nil {
		return nil, err
	}
	if err := tmp.Chmod(f.snapshot.info.Mode().Perm()); err != nil {
		return nil, err
	}
	if err := f.location.io.syncFile(tmp); err != nil {
		return nil, err
	}
	next, err := tmp.Stat()
	if err != nil {
		return nil, err
	}
	if err := f.checkPath(); err != nil {
		return nil, err
	}
	pathInfo, err := folder.Lstat(base)
	if err != nil {
		return nil, err
	}
	liveDir, err := os.Stat(parent)
	if err != nil {
		return nil, err
	}
	current := pathInfo.Sys().(*syscall.Stat_t)
	if !os.SameFile(dirInfo, liveDir) || liveDir.Mode() != dirInfo.Mode() ||
		liveDir.Sys().(*syscall.Stat_t).Uid != dirID.Uid || !os.SameFile(f.snapshot.info, pathInfo) || current.Nlink != 1 ||
		current.Uid != old.Uid || current.Gid != old.Gid || pathInfo.Mode() != f.snapshot.info.Mode() {
		return nil, ErrConfigFileChanged
	}
	if attrs, err := configDirectoryHasXattrs(f.file); err != nil || attrs {
		if err != nil {
			return nil, err
		}
		return nil, ErrConfigFileChanged
	}
	if err := folder.Rename(name, base); err != nil {
		return nil, err
	}
	committed = true
	f.published = tmp
	if err := f.location.io.syncFile(dir); err != nil {
		log.WithError(err).Warn("config committed but syncing its directory failed")
	}
	return &ConfigFileSnapshot{info: next, data: bytes.Clone(data)}, nil
}

// Linux can invalidate security metadata on a content change even when the
// original inode is retained. Check the locked descriptor before either save.
func checkConfigWriteIntegrity(file *os.File) error {
	fd := int(file.Fd())
	return checkConfigIntegrityAttributes(func(names []byte) (int, error) {
		return unix.Flistxattr(fd, names)
	})
}

func checkConfigIntegrityAttributes(list func([]byte) (int, error)) error {
	size, err := list(nil)
	if errors.Is(err, unix.EOPNOTSUPP) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect configuration integrity attributes: %w", err)
	}
	if size == 0 {
		return nil
	}
	names := make([]byte, size)
	n, err := list(names)
	if err != nil {
		return fmt.Errorf("read configuration integrity attributes: %w", err)
	}
	for _, name := range bytes.Split(names[:n], []byte{0}) {
		switch string(name) {
		case "security.capability", "security.ima", "security.evm":
			return fmt.Errorf("cannot safely preserve configuration integrity attribute %s", name)
		}
	}
	return nil
}

func configDirectoryHasXattrs(file *os.File) (bool, error) {
	size, err := unix.Flistxattr(int(file.Fd()), nil)
	if errors.Is(err, unix.EOPNOTSUPP) {
		return false, nil
	}
	return size != 0, err
}

func sameConfigMount(a, b *os.File) bool {
	var first, second unix.Statx_t
	mask := unix.STATX_MNT_ID
	return unix.Statx(int(a.Fd()), "", unix.AT_EMPTY_PATH, mask, &first) == nil &&
		unix.Statx(int(b.Fd()), "", unix.AT_EMPTY_PATH, mask, &second) == nil &&
		first.Mask&uint32(mask) != 0 && second.Mask&uint32(mask) != 0 && first.Mnt_id == second.Mnt_id
}

func knownLocalConfigFilesystem(dir *os.File) bool {
	var fs unix.Statfs_t
	if unix.Fstatfs(int(dir.Fd()), &fs) != nil {
		return false
	}
	switch fs.Type {
	case unix.EXT4_SUPER_MAGIC, unix.XFS_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC, unix.OVERLAYFS_SUPER_MAGIC, unix.TMPFS_MAGIC:
		return true
	default:
		return false
	}
}
