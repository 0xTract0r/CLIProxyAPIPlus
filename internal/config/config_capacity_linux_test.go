package config

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOrdinaryConfigCapacityFailurePreservesBothYAMLPaths(t *testing.T) {
	if args := flag.Args(); len(args) != 1 || args[0] != "capacity-probe" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestOrdinaryConfigCapacityFailurePreservesBothYAMLPaths$", "--", "capacity-probe")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("Linux capacity probe: %v\n%s", err, output)
		}
		return
	}
	dir := t.TempDir()
	original := []byte("# keep exact original\nport: 8317\nhost: " + strings.Repeat("o", 8192) + "\n")
	originalMTime := time.Unix(1000, 0)
	targets := []string{filepath.Join(dir, "full.yaml"), filepath.Join(dir, "scalar.yaml")}
	for _, path := range targets {
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, originalMTime, originalMTime); err != nil {
			t.Fatal(err)
		}
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	limit.Cur = 4096
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(unix.SIGXFSZ)
	next := strings.Repeat("n", 8192)
	for i, save := range []func(string) error{
		func(path string) error { return SaveConfigPreserveComments(path, &Config{Port: 8317, Host: next}) },
		func(path string) error {
			return SaveConfigPreserveCommentsUpdateNestedScalar(path, []string{"host"}, next)
		},
	} {
		path := targets[i]
		if err := save(path); !errors.Is(err, unix.EFBIG) {
			t.Errorf("method %d: err=%v", i, err)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, original) {
			t.Errorf("method %d changed prior bytes: len=%d err=%v", i, len(after), err)
		}
		info, err := os.Stat(path)
		if err != nil || !info.ModTime().Equal(originalMTime) {
			t.Errorf("method %d touched the destination despite preflight-visible limit: %v", i, err)
		}
	}
}
