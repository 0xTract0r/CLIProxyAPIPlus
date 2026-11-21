package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDirectorySaveLinuxHandoff(t *testing.T) {
	if child := os.Getenv("HANDOFF_CHILD"); child != "" {
		testHandoffChild(t, child, os.Getenv("HANDOFF_CONFIG"))
		return
	}
	t.Setenv(linuxDirectorySaveEnv, "1")
	for _, scenario := range []string{"writer-open-old", "writer-open-new", "reader-open-old", "reader-open-new"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			const before = "port: 18317\n"
			const draft = "port: 18417\n"
			if err := os.WriteFile(path, []byte(before), 0640); err != nil {
				t.Fatal(err)
			}
			oldSnapshotFD, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer oldSnapshotFD.Close()

			var proc *exec.Cmd
			var marker <-chan string
			defer func() {
				if proc != nil {
					_ = proc.Process.Kill()
					_ = proc.Wait()
				}
			}()
			spawn := func(info os.FileInfo) {
				var stdout bytes.Buffer
				proc = exec.Command(os.Args[0], "-test.run", "^TestDirectorySaveLinuxHandoff$", "-test.v")
				proc.Env = append(os.Environ(), "HANDOFF_CHILD="+scenario, "HANDOFF_CONFIG="+path)
				if strings.HasPrefix(scenario, "writer") {
					proc.ExtraFiles = []*os.File{oldSnapshotFD}
				}
				out, err := proc.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				proc.Stderr = &stdout
				if err := proc.Start(); err != nil {
					t.Fatal(err)
				}
				lines := make(chan string, 128)
				go func() {
					defer close(lines)
					scan := bufio.NewScanner(out)
					for scan.Scan() {
						lines <- scan.Text()
					}
				}()
				marker = lines
				until := time.Now().Add(3 * time.Second)
				for time.Now().Before(until) {
					found, _ := filepath.Glob(fmt.Sprintf("/proc/%d/fd/*", proc.Process.Pid))
					for _, fd := range found {
						if filepath.Base(fd) == "3" && len(proc.ExtraFiles) > 0 {
							continue
						}
						observed, err := os.Stat(fd)
						if err == nil && os.SameFile(info, observed) {
							return
						}
					}
					time.Sleep(2 * time.Millisecond)
				}
				t.Fatalf("child %s never opened the expected inode: %s", scenario, stdout.String())
			}
			oldInfo, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := ReadConfigFileSnapshot(path)
			if err != nil {
				t.Fatal(err)
			}
			isOld := strings.HasSuffix(scenario, "old")
			if err := withConfigFile(path, configWriteIfPossible, func(f *lockedConfigFile) error {
				if isOld {
					spawn(oldInfo)
				}
				committed, err := f.replace([]byte(draft))
				if err != nil {
					return err
				}
				info := committed.info
				if os.SameFile(oldInfo, info) || string(committed.Bytes()) != draft || info.Mode().Perm() != 0640 {
					return fmt.Errorf("atomic save returned an incorrect snapshot")
				}
				if committed.SourceHash() != ComputeSourceHash([]byte(draft)) {
					return fmt.Errorf("snapshot hash mismatch")
				}

				if !isOld {
					spawn(info)
				}
				if bytes, err := os.ReadFile(path); err != nil || string(bytes) != draft {
					return fmt.Errorf("unlocked path read did not see complete new bytes: %v", err)
				}
				until := time.After(80 * time.Millisecond)
				for {
					select {
					case line, ok := <-marker:
						if !ok || strings.HasPrefix(line, "RESULT:") {
							t.Fatalf("child crossed a held inode lock: %q", line)
						}
					case <-until:
						t.Logf("%s opened real target fd and had no result while both inode locks were held", scenario)
						return nil
					}
				}
			}); err != nil {
				t.Fatal(err)
			}

			var result string
			for line := range marker {
				if strings.HasPrefix(line, "RESULT:") {
					result = line
				}
			}
			if err := proc.Wait(); err != nil {
				t.Fatalf("child failed: %v (%s)", err, result)
			}
			proc = nil
			t.Log(result)
			if isOld && strings.HasPrefix(scenario, "writer") && !strings.Contains(result, "stale=true") {
				t.Fatalf("old opened descriptor should report stale: %s", result)
			}
			if strings.HasPrefix(scenario, "reader") && (!strings.Contains(result, "stale=false") || !strings.Contains(result, "18417")) {
				t.Fatal(result)
			}
			if strings.HasPrefix(scenario, "writer") && !strings.Contains(result, "retry=true") {
				t.Fatal(result)
			}
			final, err := ReadConfigFileSnapshot(path)
			if err != nil {
				t.Fatal(err)
			}
			if os.SameFile(final.info, expected.info) {
				t.Fatal("new path retained old identity")
			}
		})
	}
}

func testHandoffChild(t *testing.T, mode, path string) {
	if strings.HasPrefix(mode, "reader") {
		snapshot, err := ReadConfigFileSnapshot(path)
		if errors.Is(err, ErrConfigFileChanged) {
			fmt.Println("RESULT: stale=true")
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("RESULT: stale=false data=%q\n", snapshot.Bytes())
		return
	}
	file := os.NewFile(3, "old-fixture")
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	expect := &ConfigFileSnapshot{info: info, data: raw}
	_, err = WriteConfigFileCAS(path, expect, []byte("port: 18457\n"))
	if !errors.Is(err, ErrConfigFileChanged) {
		t.Fatalf("stale CAS result: %v", err)
	}
	latest, err := ReadConfigFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(latest.Bytes()) != "port: 18417\n" {
		t.Fatal("retry snapshot did not include first write")
	}
	next := append(latest.Bytes(), []byte("host: localhost\n")...)
	if _, err := WriteConfigFileCAS(path, latest, next); err != nil {
		t.Fatal(err)
	}
	got, err := ReadConfigFileSnapshot(path)
	if err != nil || !bytes.Equal(got.Bytes(), next) {
		t.Fatalf("retry did not persist both edits: %v", err)
	}
	fmt.Println("RESULT: stale=true retry=true")
}

func TestDirectorySaveLinuxNonreplaceableFile(t *testing.T) {
	t.Setenv(linuxDirectorySaveEnv, "1")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	alias := filepath.Join(dir, "alias.yaml")
	if err := os.WriteFile(path, []byte("port: 18317\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	before, err := ReadConfigFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	draft := []byte("port: 18417\n")
	got, err := WriteConfigFileCAS(path, before, draft)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(alias)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(alias)
	if err != nil || !os.SameFile(before.info, info) || !os.SameFile(got.info, info) || !bytes.Equal(raw, draft) || info.Mode().Perm() != 0640 {
		t.Fatalf("in-place CAS changed inode/mode or alias did not see data: %v", err)
	}
}

func TestDirectorySaveLinuxVersionedSnapshot(t *testing.T) {
	t.Setenv(linuxDirectorySaveEnv, "1")
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("port: 18317\n# keep this comment\n---\nextra: keep\n")
	if err := os.WriteFile(path, raw, 0640); err != nil {
		t.Fatal(err)
	}
	before, err := ReadConfigFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil || cfg.ServerID == "" {
		t.Fatalf("startup ID: %v", err)
	}
	id := cfg.ServerID
	committed, err := ReadConfigFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before.info, committed.info) || cfg.SourceHash != committed.SourceHash() ||
		!bytes.Contains(committed.data, []byte("# keep this comment")) || !bytes.HasSuffix(committed.data, []byte("---\nextra: keep\n")) {
		t.Fatal("committed snapshot did not contain replacement identity and full YAML stream")
	}
	cfg, err = LoadConfig(path)
	if err != nil || cfg.ServerID != id {
		t.Fatalf("server ID changed after replacement: %v", err)
	}
}

func TestDirectorySaveLinuxDefaultACL(t *testing.T) {
	t.Setenv(linuxDirectorySaveEnv, "1")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("port: 18317\n"), 0640); err != nil {
		t.Fatal(err)
	}
	testSetACL(t, dir, defaultACL, namedReaderACL())
	before, err := ReadConfigFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	next, err := WriteConfigFileCAS(path, before, []byte("port: 18417\n"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before.info, next.info) || next.info.Mode().Perm() != 0640 {
		t.Fatal("default-ACL directory did not use an eligible ordinary replacement")
	}
	if _, err := testGetACL(path, accessACL); !errors.Is(err, unix.ENODATA) {
		t.Fatalf("inherited ACL after replacement: %v", err)
	}
}

func TestDirectorySaveLinuxSingleBind(t *testing.T) {
	path := os.Getenv("DIRECTORY_SAVE_BIND_FIXTURE")
	if path == "" {
		t.Skip("requires the isolated Linux bind fixture")
	}
	t.Setenv(linuxDirectorySaveEnv, "1")
	before, err := ReadConfigFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if sameConfigMount(file, dir) {
		t.Fatal("fixture is not a single-file mount")
	}
	next, err := WriteConfigFileCAS(path, before, []byte("port: 18417\n"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := ReadConfigFileSnapshot(path)
	if err != nil || !os.SameFile(before.info, after.info) || !os.SameFile(next.info, after.info) ||
		before.info.Mode() != after.info.Mode() || !bytes.Equal(next.data, after.data) {
		t.Fatalf("bind-backed save changed identity or mode: %v", err)
	}
}

func BenchmarkDirectorySaveLinux(b *testing.B) {
	for _, mode := range []string{"0", "1"} {
		b.Run(mode, func(b *testing.B) {
			b.Setenv(linuxDirectorySaveEnv, mode)
			path := filepath.Join(b.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("port: 18317\n"), 0640); err != nil {
				b.Fatal(err)
			}
			snap, err := ReadConfigFileSnapshot(path)
			if err != nil {
				b.Fatal(err)
			}
			replacements := [][]byte{[]byte("port: 18417\n"), []byte("port: 18317\n")}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				snap, err = WriteConfigFileCAS(path, snap, replacements[i%2])
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestDirectorySaveLinuxUnopenableParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires non-root")
	}
	t.Setenv(linuxDirectorySaveEnv, "1")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("port: 18317\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0300); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	before, err := ReadConfigFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	next, err := WriteConfigFileCAS(path, before, []byte("port: 18417\n"))
	if err != nil || !os.SameFile(before.info, next.info) {
		t.Fatalf("fallback did not retain the original file: %v", err)
	}
}

func TestDirectorySaveLinuxReadOnlyPreservesSnapshot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires non-root")
	}
	t.Setenv(linuxDirectorySaveEnv, "1")
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("port: 18317\n")
	if err := os.WriteFile(path, raw, 0400); err != nil {
		t.Fatal(err)
	}
	before, err := ReadConfigFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = WriteConfigFileCAS(path, before, []byte("port: 18417\n")); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("readonly CAS returned %v", err)
	}
	after, err := ReadConfigFileSnapshot(path)
	if err != nil || !os.SameFile(before.info, after.info) || after.info.Mode().Perm() != 0400 || !bytes.Equal(raw, after.data) {
		t.Fatalf("readonly CAS changed inode, mode or data: %v", err)
	}
}

// Test each commit against the immediately previous snapshot, including after a
// shrinking save. The mounted fixture is supplied by the Linux integration run.
func TestConfigReplacementPreservesMountSemantics(t *testing.T) {
	t.Setenv(linuxDirectorySaveEnv, "1")
	t.Run("directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("port: 18317\n# initial comment\n"), 0640); err != nil {
			t.Fatal(err)
		}
		testTwoConfigSaves(t, path, false)
	})
	t.Run("single file bind", func(t *testing.T) {
		path := os.Getenv("CONFIG_TWO_SAVES_BIND_FIXTURE")
		if path == "" {
			t.Skip("requires the isolated Linux single-file bind fixture")
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		parent, err := os.Open(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		defer parent.Close()
		if sameConfigMount(file, parent) {
			t.Fatal("fixture is not a single-file bind")
		}
		testTwoConfigSaves(t, path, true)
	})
}

func testTwoConfigSaves(t *testing.T, path string, preserveInode bool) {
	t.Helper()
	before, err := ReadConfigFileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	original := before.info.Sys().(*syscall.Stat_t)
	for step, replacement := range [][]byte{[]byte("port: 9001\n"), []byte("port: 9002\n# growth from prior\n")} {
		if (step == 0 && len(replacement) >= len(before.data)) ||
			(step == 1 && len(replacement) <= len(before.data)) {
			t.Fatal("fixture must exercise truncation followed by growth")
		}
		committed, err := WriteConfigFileCAS(path, before, replacement)
		if err != nil {
			t.Fatal(err)
		}
		after, err := ReadConfigFileSnapshot(path)
		if err != nil {
			t.Fatal(err)
		}
		metadata := after.info.Sys().(*syscall.Stat_t)
		if os.SameFile(before.info, after.info) != preserveInode ||
			!os.SameFile(committed.info, after.info) ||
			metadata.Uid != original.Uid || metadata.Gid != original.Gid ||
			metadata.Mode != original.Mode || !bytes.Equal(after.data, replacement) {
			t.Fatalf("commit changed expected inode policy, metadata or contents: %q", replacement)
		}
		before = after
	}
}
