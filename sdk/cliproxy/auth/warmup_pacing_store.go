package auth

import (
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// WarmupPacingFileStore is a single-instance sidecar store on the durable auth
// volume. Its .pacing suffix deliberately excludes it from auth JSON discovery.
type WarmupPacingFileStore struct{ dir string }

func NewWarmupPacingFileStore(dir string) (*WarmupPacingFileStore, error) {
	if dir == "" {
		return nil, errors.New("warmup pacing state directory is empty")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("warmup pacing state path is not a directory")
	}
	return &WarmupPacingFileStore{dir: dir}, nil
}

func (s *WarmupPacingFileStore) path(key string) (string, error) {
	if len(key) != 64 {
		return "", errors.New("invalid pacing account digest")
	}
	if _, err := hex.DecodeString(key); err != nil {
		return "", errors.New("invalid pacing account digest")
	}
	return filepath.Join(s.dir, key+".pacing"), nil
}

func (s *WarmupPacingFileStore) Load(key string) ([]byte, error) {
	path, err := s.path(key)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > warmupPacingMaxBytes {
		return nil, errors.New("unsafe warmup pacing sidecar mode or size")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, warmupPacingMaxBytes+1))
	if err == nil && len(data) > warmupPacingMaxBytes {
		err = errors.New("warmup pacing sidecar exceeds size limit")
	}
	return data, err
}

func (s *WarmupPacingFileStore) Save(key string, data []byte) error {
	path, err := s.path(key)
	if err != nil {
		return err
	}
	if len(data) > warmupPacingMaxBytes {
		return errors.New("warmup pacing sidecar exceeds size limit")
	}
	f, err := os.CreateTemp(s.dir, ".pacing-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}
