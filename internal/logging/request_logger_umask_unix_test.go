//go:build unix

package logging

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestCompressedRequestLogRespectsUmask(t *testing.T) {
	const wantModeEnv = "TEST_REQUEST_LOG_EXPECTED_MODE"
	if raw := os.Getenv(wantModeEnv); raw != "" {
		mode, err := strconv.ParseUint(raw, 8, 32)
		if err != nil {
			t.Fatal(err)
		}
		checkCompressedRequestLogModes(t, os.FileMode(mode))
		return
	}
	for _, tc := range []struct{ mask, mode string }{{"022", "0644"}, {"077", "0600"}} {
		t.Run(tc.mask, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.Command("sh", "-c", "umask \"$1\" && exec \"$2\" -test.run '^TestCompressedRequestLogRespectsUmask$'", "umask-test", tc.mask, executable)
			child.Env = append(os.Environ(), wantModeEnv+"="+tc.mode)
			if output, err := child.CombinedOutput(); err != nil {
				t.Fatalf("under umask %s: %v\n%s", tc.mask, err, output)
			}
		})
	}
}

func checkCompressedRequestLogModes(t *testing.T, wantMode os.FileMode) {
	dir := t.TempDir()
	target := filepath.Join(dir, "success.log")
	assertMode := func(path string) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Fatalf("%s mode = %04o, want %04o", filepath.Base(path), got, wantMode)
		}
	}
	if err := writeCompressedRequestLog(target, func(w io.Writer) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("expected one unpublished temporary file, got %v", entries)
		}
		assertMode(filepath.Join(dir, entries[0].Name()))
		_, err = io.WriteString(w, "synthetic completed log")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertMode(target + ".gz")
	writeFailure := errors.New("synthetic failed write")
	for _, phase := range []string{"write", "publish"} {
		path := filepath.Join(dir, phase+".log")
		if phase == "publish" {
			if err := os.Mkdir(path+".gz", 0o700); err != nil {
				t.Fatal(err)
			}
		}
		err := writeCompressedRequestLog(path, func(w io.Writer) error {
			if _, err := io.WriteString(w, "synthetic partial log"); err != nil {
				return err
			}
			if phase == "write" {
				return writeFailure
			}
			return nil
		})
		if err == nil || (phase == "write" && !errors.Is(err, writeFailure)) {
			t.Fatalf("%s failure = %v", phase, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && entry.Name() != "success.log.gz" {
			t.Fatalf("unexpected file left behind: %s", entry.Name())
		}
	}
}
