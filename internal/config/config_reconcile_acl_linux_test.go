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
