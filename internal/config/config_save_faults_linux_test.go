package config

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func saveFaultFixture(t *testing.T, atomic string) (string, *ConfigFileSnapshot, []byte) {
	t.Helper()
	t.Setenv(linuxDirectorySaveEnv, atomic)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 18317\n# original\n"), 0640); err != nil {
		t.Fatal(err)
	}
	before, err := ReadConfigFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, before, []byte("port: 28517\n")
}

func TestConfigSaveCASInodeFailure(t *testing.T) {
	for _, name := range []string{"short write", "partial EIO", "sync EIO", "restore EIO"} {
		t.Run(name, func(t *testing.T) {
			path, before, replacement := saveFaultFixture(t, "0")
			var writes, syncs int
			var cause error = syscall.EIO
			if name == "short write" {
				cause = io.ErrShortWrite
			}
			ops := configSaveIO{
				writeAt: func(file *os.File, data []byte, offset int64) (int, error) {
					writes++
					if writes == 1 && name != "sync EIO" {
						n, err := file.WriteAt(data[:7], offset)
						if err != nil || n != 7 {
							t.Fatalf("real prefix write: %d, %v", n, err)
						}
						onDisk, err := os.ReadFile(path)
						if err != nil || bytes.Equal(onDisk, before.data) {
							t.Fatalf("prefix did not change file: %v", err)
						}
						if name == "short write" {
							return n, nil
						}
						return n, syscall.EIO
					}
					if writes == 2 && name == "restore EIO" {
						return 0, syscall.ENOSPC
					}
					return file.WriteAt(data, offset)
				},
				sync: func(file *os.File) error {
					syncs++
					if syncs == 1 && name == "sync EIO" {
						onDisk, err := os.ReadFile(path)
						if err != nil || !bytes.Equal(onDisk, replacement) {
							t.Fatalf("sync fixture did not write and truncate replacement: %v", err)
						}
						return syscall.EIO
					}
					return file.Sync()
				},
			}
			committed, err := writeConfigFileCAS(configFileLocation{path: path, io: ops}, before, replacement)
			if committed != nil || !errors.Is(err, cause) || writes != 2 {
				t.Fatalf("committed=%v writes=%d err=%v", committed != nil, writes, err)
			}
			after, readErr := ReadConfigFileSnapshot(path)
			if readErr != nil || !os.SameFile(before.info, after.info) || after.info.Mode().Perm() != before.info.Mode().Perm() {
				t.Fatalf("failed save changed inode or mode: %v", readErr)
			}
			if name == "restore EIO" {
				if !errors.Is(err, syscall.ENOSPC) || bytes.Equal(after.data, before.data) {
					t.Fatalf("expected joined cause and unrestored bytes: %v", err)
				}
				return
			}
			if !bytes.Equal(after.data, before.data) || syncs == 0 {
				t.Fatal("rollback did not restore and synchronize original bytes")
			}
			if _, err := WriteConfigFileCAS(path, before, replacement); err != nil {
				t.Fatalf("retry against original snapshot: %v", err)
			}
		})
	}
}

func TestConfigSaveCASDirectorySyncFailure(t *testing.T) {
	for _, afterRename := range []bool{false, true} {
		name := "temporary file sync"
		if afterRename {
			name = "directory sync after rename"
		}
		t.Run(name, func(t *testing.T) {
			path, before, replacement := saveFaultFixture(t, "1")
			syncs := 0
			ops := configSaveIO{sync: func(file *os.File) error {
				syncs++
				info, err := file.Stat()
				if err != nil {
					t.Fatal(err)
				}
				if info.IsDir() != afterRename {
					return file.Sync()
				}
				onDisk, err := os.ReadFile(path)
				want := before.data
				if afterRename {
					want = replacement
				}
				if err != nil || !bytes.Equal(onDisk, want) {
					t.Fatalf("wrong bytes visible at sync: %v", err)
				}
				return syscall.EIO
			}}
			committed, err := writeConfigFileCAS(configFileLocation{path: path, io: ops}, before, replacement)
			disk, readErr := ReadConfigFileSnapshot(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if afterRename {
				if err != nil || committed == nil || syncs != 2 || os.SameFile(before.info, disk.info) ||
					!os.SameFile(committed.info, disk.info) || !bytes.Equal(disk.data, replacement) ||
					committed.SourceHash() != disk.SourceHash() || disk.info.Mode().Perm() != before.info.Mode().Perm() {
					t.Fatalf("directory warning changed commit result: syncs=%d err=%v", syncs, err)
				}
			} else if !errors.Is(err, syscall.EIO) || committed != nil || syncs != 1 ||
				!os.SameFile(before.info, disk.info) || !bytes.Equal(before.data, disk.data) {
				t.Fatalf("temporary sync published an incomplete operation: syncs=%d err=%v", syncs, err)
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary save file leaked: %v, %v", entries, err)
			}
		})
	}
}

func TestConfigSaveCASPreservesExternalMarker(t *testing.T) {
	path, before, replacement := saveFaultFixture(t, "1")
	const attr = "user.cliproxy-label"
	marker := []byte("synthetic-marker")
	if err := unix.Setxattr(path, attr, marker, 0); errors.Is(err, unix.ENOTSUP) {
		t.Skip(err)
	} else if err != nil {
		t.Fatal(err)
	}
	after, err := WriteConfigFileCAS(path, before, replacement)
	if err != nil || !os.SameFile(before.info, after.info) || !bytes.Equal(after.data, replacement) {
		t.Fatalf("marked config did not commit on its original inode: %v", err)
	}
	got := make([]byte, len(marker))
	n, err := unix.Getxattr(path, attr, got)
	if err != nil || n != len(marker) || !bytes.Equal(got, marker) {
		t.Fatalf("external marker changed: %v", err)
	}
}
