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

	"golang.org/x/sys/unix"
)

func TestSaveConfigPreservesBytesOnFileSizeLimit(t *testing.T) {
	// RLIMIT_FSIZE and SIGXFSZ are process-wide. Change them only in the probe.
	const child = "config-save-file-size-probe"
	if args := flag.Args(); len(args) != 1 || args[0] != child {
		cmd := exec.CommandContext(t.Context(), os.Args[0],
			"-test.run=^TestSaveConfigPreservesBytesOnFileSizeLimit$", "-test.timeout=10s", "--", child)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("file size probe failed: %v\n%s", err, output)
		}
		return
	}

	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	capped := limit
	capped.Cur = min(capped.Cur, 4096)
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &capped); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(unix.SIGXFSZ)
	t.Cleanup(func() {
		if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
			t.Error(err)
		}
		signal.Reset(unix.SIGXFSZ)
	})

	old := []byte("# preserve this comment\nport: 2101\nhost: localhost\n")
	long := strings.Repeat("x", 8192)
	for _, tt := range []struct {
		name string
		save func(string) error
	}{
		{"full", func(path string) error {
			return SaveConfigPreserveComments(path, &Config{Port: 2101, Host: long})
		}},
		{"scalar", func(path string) error {
			return SaveConfigPreserveCommentsUpdateNestedScalar(path, []string{"host"}, long)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if fixture := os.Getenv("ROLLBACK_BIND_FIXTURE"); fixture != "" {
				path = fixture
			}
			if err := os.WriteFile(path, old, 0600); err != nil {
				t.Fatal(err)
			}
			var alias string
			if os.Getenv("ROLLBACK_HARD_LINK_FIXTURE") == "yes" {
				alias = path + ".alias"
				if err := os.Link(path, alias); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(alias)
			}
			if err := tt.save(path); !errors.Is(err, unix.EFBIG) {
				t.Fatalf("save error = %v, want EFBIG", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, old) {
				t.Fatalf("failed save changed configuration bytes: size = %d, want %d", len(got), len(old))
			}
			if alias != "" {
				gotAlias, err := os.ReadFile(alias)
				if err != nil || !bytes.Equal(gotAlias, old) {
					t.Fatalf("failed save changed hard link: size=%d err=%v", len(gotAlias), err)
				}
			}
		})
	}
}
