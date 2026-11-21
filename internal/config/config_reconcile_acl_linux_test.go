package config

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReconcileConfigSavePreservesNamedReaderAndDeniesOwningGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := namedReaderACL()
	testSetACL(t, path, accessACL, want)
	before, err := os.Stat(path)
	if err != nil || before.Mode().Perm() != 0o640 {
		t.Fatalf("fixture ACL mask mode did not equal 0640: %v", err)
	}
	if err := SaveConfigPreserveComments(path, &Config{Port: 8318}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := testGetACL(path, accessACL)
	if err != nil || !bytes.Equal(got, want) || !os.SameFile(before, after) || after.Mode().Perm() != 0o640 {
		t.Fatalf("ACL/mode/inode changed; named reader or group permissions changed: %v", err)
	}
	readerID := binary.LittleEndian.Uint32(got[16:20])
	namedAccess := binary.LittleEndian.Uint16(got[14:16])
	groupAccess := binary.LittleEndian.Uint16(got[22:24])
	if readerID != 12345 || namedAccess != 4 || groupAccess != 0 {
		t.Fatalf("named uid=%d perms=%d owning group perms=%d", readerID, namedAccess, groupAccess)
	}
}

func TestReconcileConfigSaveDoesNotInheritNewAccessACL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	testSetACL(t, dir, defaultACL, namedReaderACL())
	// Prove this filesystem would inherit the default ACL on the temp inode.
	probe, err := os.CreateTemp(dir, "inheritance-probe-")
	if err != nil {
		t.Fatal(err)
	}
	probe.Close()
	if _, err = testGetACL(probe.Name(), accessACL); err != nil {
		t.Fatalf("default ACL not inherited: %v", err)
	}
	if err := SaveConfigPreserveComments(path, &Config{Port: 8318}); err != nil {
		t.Fatal(err)
	}
	if acl, err := testGetACL(path, accessACL); !errors.Is(err, unix.ENODATA) {
		t.Fatalf("configuration acquired inherited ACL (%x): %v", acl, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("original mode not preserved: %v", err)
	}
}

const accessACL = "system.posix_acl_access"
const defaultACL = "system.posix_acl_default"

func namedReaderACL() []byte {
	// Linux xattr ACL v2, owner rw, group-object none, named UID read,
	// mask read. The reported 0640 group bits are the ACL mask.
	return []byte{
		2, 0, 0, 0,
		1, 0, 6, 0, 255, 255, 255, 255,
		2, 0, 4, 0, 0x39, 0x30, 0, 0,
		4, 0, 0, 0, 255, 255, 255, 255,
		16, 0, 4, 0, 255, 255, 255, 255,
		32, 0, 0, 0, 255, 255, 255, 255,
	}
}

func testSetACL(t *testing.T, path, name string, acl []byte) {
	t.Helper()
	if err := unix.Setxattr(path, name, acl, 0); err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) {
			t.Skip("fixture filesystem lacks ACL support")
		}
		t.Fatal(err)
	}
}

func testGetACL(path, kind string) ([]byte, error) {
	buf := make([]byte, 1024)
	n, err := unix.Getxattr(path, kind, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func TestConfigSaveXattrsUnsupportedKeepsOrdinaryReplacement(t *testing.T) {
	if os.Getenv("EXPECT_CONFIG_FS_NO_ACL") != "1" {
		t.Skip("requires a filesystem without POSIX ACL support")
	}
	t.Setenv(linuxDirectorySaveEnv, "1")
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("port: 8317\n")
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(path, accessACL, namedReaderACL(), 0); !errors.Is(err, unix.EOPNOTSUPP) {
		t.Fatalf("expected filesystem ACL EOPNOTSUPP, got %v", err)
	}
	before, err := ReadConfigFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	next := []byte("port: 8318\n")
	committed, err := WriteConfigFileCAS(path, before, next)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || os.SameFile(before.info, after) || !os.SameFile(committed.info, after) || after.Mode().Perm() != 0o640 {
		t.Fatalf("ordinary replacement did not retain mode and replace inode: %v", err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, next) {
		t.Fatalf("replacement bytes=%q err=%v", actual, err)
	}
}

func TestConfigACLReaderFixture(t *testing.T) {
	path := os.Getenv("CONFIG_ACL_READER_FIXTURE")
	if path == "" {
		t.Skip("requires uid-isolated container fixture")
	}
	t.Setenv(linuxDirectorySaveEnv, "1")
	if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(path, accessACL, namedReaderACL(), 0); err != nil {
		t.Fatal(err)
	}
	before, err := ReadConfigFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := WriteConfigFileCAS(path, before, []byte("port: 8318\n"))
	if err != nil || !os.SameFile(before.info, committed.info) {
		t.Fatalf("ACL fixture must retain its original inode: %v", err)
	}
}
