package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WithFileLockInRoot coordinates with ordinary refresh writers using the same
// lock name. It opens the lock beneath an already pinned credential root.
func WithFileLockInRoot(root *os.Root, name string, fn func() error) error {
	if !filepath.IsLocal(name) || name != filepath.Base(name) || name == "." {
		return errors.New("invalid scoped credential lock name")
	}
	canonicalDir, err := filepath.EvalSymlinks(root.Name())
	if err != nil {
		return err
	}
	fromRoot, err := root.Stat(".")
	if err != nil {
		return err
	}
	fromPath, err := os.Stat(canonicalDir)
	if err != nil || !os.SameFile(fromPath, fromRoot) {
		return fmt.Errorf("credential lock directory changed")
	}

	mu := lockForCanonicalFilePath(filepath.Join(canonicalDir, name))
	mu.Lock()
	defer mu.Unlock()

	lockName := name + ".lock"
	expected, err := root.Lstat(lockName)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if expected != nil && !expected.Mode().IsRegular() {
		return errors.New("credential lock must be a regular file")
	}
	release, err := lockOpenedFile(root.OpenFile(lockName, os.O_CREATE|os.O_RDWR, 0o600))
	if err != nil {
		return err
	}
	defer release()
	return fn()
}
