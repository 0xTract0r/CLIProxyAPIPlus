package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/sys/unix"
)

func guardOrdinaryCredentialMetadata(file *os.File, path string) (*ordinaryCredentialMetadataGuard, error) {
	original, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: stat credential: %v", ErrUnsafeCredentialMetadata, err)
	}
	old := original.Sys().(*syscall.Stat_t)
	if !original.Mode().IsRegular() || original.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return nil, fmt.Errorf("%w: unsupported file type or mode", ErrUnsafeCredentialMetadata)
	}
	if old.Nlink != 1 || int(old.Uid) != os.Geteuid() {
		return nil, fmt.Errorf("%w: unsupported link count or owner", ErrUnsafeCredentialMetadata)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("%w: open parent: %v", ErrUnsafeCredentialMetadata, err)
	}
	defer dir.Close()
	dirInfo, err := dir.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: stat parent: %v", ErrUnsafeCredentialMetadata, err)
	}
	var oldMount, parentMount unix.Statx_t
	const mask = unix.STATX_MNT_ID
	if unix.Statx(int(file.Fd()), "", unix.AT_EMPTY_PATH, mask, &oldMount) != nil ||
		unix.Statx(int(dir.Fd()), "", unix.AT_EMPTY_PATH, mask, &parentMount) != nil ||
		oldMount.Mask&mask == 0 || parentMount.Mask&mask == 0 || oldMount.Mnt_id != parentMount.Mnt_id {
		return nil, fmt.Errorf("%w: file mount differs from parent or cannot be checked", ErrUnsafeCredentialMetadata)
	}
	if err := checkOrdinaryCredentialXattrs(file); err != nil {
		return nil, err
	}
	return &ordinaryCredentialMetadataGuard{
		checkTemporary: checkOrdinaryCredentialXattrs,
		setMetadata: func(tmp *os.File) error {
			info, err := tmp.Stat()
			if err != nil {
				return fmt.Errorf("%w: stat temporary file: %v", ErrUnsafeCredentialMetadata, err)
			}
			current := info.Sys().(*syscall.Stat_t)
			if current.Uid != old.Uid || current.Gid != old.Gid {
				if err := tmp.Chown(int(old.Uid), int(old.Gid)); err != nil {
					return fmt.Errorf("%w: retain file owner/group: %v", ErrUnsafeCredentialMetadata, err)
				}
			}
			if err := tmp.Chmod(original.Mode().Perm()); err != nil {
				return fmt.Errorf("%w: retain file mode: %v", ErrUnsafeCredentialMetadata, err)
			}
			info, err = tmp.Stat()
			if err != nil {
				return fmt.Errorf("%w: verify temporary file: %v", ErrUnsafeCredentialMetadata, err)
			}
			got := info.Sys().(*syscall.Stat_t)
			if got.Uid != old.Uid || got.Gid != old.Gid || info.Mode().Perm() != original.Mode().Perm() {
				return fmt.Errorf("%w: replacement metadata does not match", ErrUnsafeCredentialMetadata)
			}
			return checkOrdinaryCredentialXattrs(tmp)
		},
		checkTarget: func() error {
			current, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				return cliproxyauth.ErrCredentialFileRemoved
			}
			if err != nil || !os.SameFile(original, current) || current.Mode() != original.Mode() {
				return fmt.Errorf("%w: target identity or mode changed", ErrUnsafeCredentialMetadata)
			}
			stat := current.Sys().(*syscall.Stat_t)
			if stat.Nlink != 1 || stat.Uid != old.Uid || stat.Gid != old.Gid {
				return fmt.Errorf("%w: target links or ownership changed", ErrUnsafeCredentialMetadata)
			}
			liveDir, err := os.Stat(filepath.Dir(path))
			if err != nil || !os.SameFile(dirInfo, liveDir) {
				return fmt.Errorf("%w: target directory changed", ErrUnsafeCredentialMetadata)
			}
			return checkOrdinaryCredentialXattrs(file)
		},
	}, nil
}

// Linux ACLs appear in the extended attribute list. The temporary inode check
// also rejects an ACL or label inherited from its parent directory.
func checkOrdinaryCredentialXattrs(f *os.File) error {
	size, err := unix.Flistxattr(int(f.Fd()), nil)
	if errors.Is(err, unix.EOPNOTSUPP) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: inspect file attributes: %v", ErrUnsafeCredentialMetadata, err)
	}
	if size != 0 {
		return fmt.Errorf("%w: file has ACLs or extended attributes", ErrUnsafeCredentialMetadata)
	}
	return nil
}
