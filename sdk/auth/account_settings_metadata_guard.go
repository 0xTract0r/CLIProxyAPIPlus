package auth

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrUnsafeCredentialMetadata reports that the underlying file metadata
// cannot safely be retained during an explicit credential edit.
var ErrUnsafeCredentialMetadata = errors.New("credential file metadata cannot be safely preserved")

type ordinaryCredentialMetadataGuard struct {
	checkTemporary func(*os.File) error
	setMetadata    func(*os.File) error
	checkTarget    func() error
}

func writeFileAtomicWithMetadata(path string, data []byte, perm os.FileMode, metadata *ordinaryCredentialMetadataGuard, beforeRename func()) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	defer tmp.Close()
	if metadata != nil {
		if err = metadata.checkTemporary(tmp); err != nil {
			return err
		}
	} else if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if metadata != nil {
		if err = metadata.setMetadata(tmp); err != nil {
			return err
		}
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if beforeRename != nil {
		beforeRename()
	}
	if metadata != nil {
		if err = metadata.checkTarget(); err != nil {
			return err
		}
	}
	return os.Rename(tmpName, path)
}

// List enumerates all auth JSON files under the configured directory.
