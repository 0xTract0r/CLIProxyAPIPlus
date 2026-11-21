package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestConfigIntegrityAttributePolicy(t *testing.T) {
	for _, attr := range []string{"security.capability", "security.ima", "security.evm", "user.synthetic", "system.posix_acl_access"} {
		t.Run(attr, func(t *testing.T) {
			names := []byte(attr + "\x00")
			err := checkConfigIntegrityAttributes(func(dst []byte) (int, error) {
				if dst == nil {
					return len(names), nil
				}
				return copy(dst, names), nil
			})
			if strings.HasPrefix(attr, "security.") {
				if err == nil || !strings.Contains(err.Error(), attr) {
					t.Fatalf("protected attribute %s: %v", attr, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, cause := range []error{unix.EOPNOTSUPP, unix.EACCES, unix.EPERM, unix.ERANGE} {
		t.Run(cause.Error(), func(t *testing.T) {
			check := func(dst []byte) (int, error) {
				if dst == nil && !errors.Is(cause, unix.EOPNOTSUPP) {
					return 32, nil
				}
				return 0, cause
			}
			err := checkConfigIntegrityAttributes(check)
			if errors.Is(cause, unix.EOPNOTSUPP) {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, cause) {
				t.Fatalf("opaque attributes must stop the write: %v", err)
			}
		})
	}
}

func TestConfigIntegrityCapabilityFixture(t *testing.T) {
	dir := os.Getenv("CONFIG_INTEGRITY_FIXTURE_DIR")
	if dir == "" {
		t.Skip("requires isolated Linux capability fixture")
	}
	path := filepath.Join(dir, "config.yaml")
	original := []byte("port: 18317\nhost: localhost\n")
	attr := "security.capability"
	// A valid v2 xattr with one capability bit, on a never-executed YAML file.
	capability := []byte{1, 0, 0, 2, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if os.Getenv("CONFIG_INTEGRITY_SETUP") == "1" {
		if err := os.WriteFile(path, original, 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, 12345, 12345); err != nil {
			t.Fatal(err)
		}
		if err := unix.Setxattr(path, attr, capability, 0); err != nil {
			t.Fatal(err)
		}
		return
	}
	if os.Geteuid() != 12345 {
		t.Fatal("fixture requires the unprivileged file owner")
	}
	before, err := ReadConfigFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if observed, err := testGetACL(path, attr); err != nil || !bytes.Equal(observed, capability) {
		t.Fatalf("fixture capability absent: %x, %v", observed, err)
	}
	next := []byte("port: 18417\nhost: synthetic\n")
	if os.Getenv("CONFIG_INTEGRITY_CONTROL") == "1" {
		if err := os.WriteFile(path, next, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := testGetACL(path, attr); !errors.Is(err, unix.ENODATA) {
			t.Fatalf("expected real kernel stripping on write, got %v", err)
		}
		t.Log("unprivileged write stripped security.capability")
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, mode := range []string{"0", "1"} {
		t.Setenv(linuxDirectorySaveEnv, mode)
		for name, save := range map[string]func() error{
			"CAS": func() error {
				got, err := WriteConfigFileCAS(path, before, next)
				if got != nil {
					t.Error("rejected save returned a committed snapshot")
				}
				return err
			},
			"rooted CAS": func() error {
				return ApplyRootedConfigFileCAS(root, "config.yaml", before, next)
			},
			"full": func() error { return SaveConfigPreserveComments(path, &Config{Port: 18417}) },
			"scalar": func() error {
				return SaveConfigPreserveCommentsUpdateNestedScalar(path, []string{"host"}, "synthetic")
			},
		} {
			if err := save(); err == nil || !strings.Contains(err.Error(), attr) {
				t.Fatalf("mode %s %s: expected integrity rejection, got %v", mode, name, err)
			}
			after, err := ReadConfigFileSnapshot(path)
			if err != nil || !os.SameFile(before.info, after.info) || !bytes.Equal(original, after.data) {
				t.Fatalf("mode %s %s: original configuration changed: %v", mode, name, err)
			}
			if observed, err := testGetACL(path, attr); err != nil || !bytes.Equal(observed, capability) {
				t.Fatalf("mode %s %s: original integrity attribute changed: %x, %v", mode, name, observed, err)
			}
		}
	}
}
