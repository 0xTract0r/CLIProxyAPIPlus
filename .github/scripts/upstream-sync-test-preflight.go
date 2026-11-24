package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type deniedTest struct {
	path, name, hash, category string
}

type pullRequestEvent struct {
	PullRequest *struct {
		Head   struct{ Ref string }
		Labels []struct{ Name string }
	} `json:"pull_request"`
}

func shouldInspect(eventPath string) (bool, error) {
	data, err := os.ReadFile(eventPath)
	if err != nil {
		return false, err
	}
	var event pullRequestEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return false, err
	}
	if event.PullRequest == nil || event.PullRequest.Head.Ref == "" {
		return false, fmt.Errorf("missing pull request head")
	}
	ref := event.PullRequest.Head.Ref
	for _, prefix := range []string{"bot/upstream-sync", "codex/upstream-sync", "codex/sync-core", "codex/integrate-prod-"} {
		if strings.HasPrefix(ref, prefix) {
			return true, nil
		}
	}
	for _, label := range event.PullRequest.Labels {
		if label.Name == "upstream-sync" {
			return true, nil
		}
	}
	return false, nil
}

func inventory(path string) ([]deniedTest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tests []deniedTest
	seen := make(map[string]bool)
	for i, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.Split(line, "\t")
		if len(p) != 4 || !strings.HasSuffix(p[0], "_test.go") || !strings.HasPrefix(p[1], "Test") || p[3] == "" {
			return nil, fmt.Errorf("invalid inventory row %d", i+1)
		}
		digest, err := hex.DecodeString(p[2])
		key := p[0] + "\x00" + p[1]
		if err != nil || len(digest) != sha256.Size || seen[key] {
			return nil, fmt.Errorf("invalid or duplicate inventory row %d", i+1)
		}
		seen[key] = true
		tests = append(tests, deniedTest{p[0], p[1], strings.ToLower(p[2]), p[3]})
	}
	if len(tests) == 0 {
		return nil, fmt.Errorf("empty inventory")
	}
	return tests, nil
}

// Hash the body tokens without the declaration name, comments or spacing so a
// reviewed historical body cannot be reintroduced by copying or renaming it.
func bodyHash(body []byte) string {
	set := token.NewFileSet()
	file := set.AddFile("", set.Base(), len(body))
	var scan scanner.Scanner
	scan.Init(file, body, nil, 0)
	h := sha256.New()
	for {
		_, tok, lit := scan.Scan()
		if tok == token.EOF {
			break
		}
		fmt.Fprintf(h, "%d:%d:%s\n", tok, len(lit), lit)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func testBodies(name string, source []byte, visit func(string, token.Position, string)) error {
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, name, source, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	for _, decl := range file.Decls {
		f, ok := decl.(*ast.FuncDecl)
		if !ok || f.Recv != nil || f.Body == nil || !strings.HasPrefix(f.Name.Name, "Test") {
			continue
		}
		start, end := set.Position(f.Body.Pos()), set.Position(f.Body.End())
		visit(f.Name.Name, start, bodyHash(source[start.Offset:end.Offset]))
	}
	return nil
}

func scanTests(list []deniedTest) (int, error) {
	names := make(map[string]deniedTest, len(list))
	bodies := make(map[string]deniedTest, len(list))
	for _, test := range list {
		names[test.path+"\x00"+test.name] = test
		bodies[test.hash] = test
	}
	output, err := exec.Command("git", "ls-files", "-z", "--", "*_test.go").Output()
	if err != nil {
		return 0, fmt.Errorf("list tracked Go tests: %w", err)
	}
	blocked := 0
	for _, item := range bytes.Split(output, []byte{0}) {
		name := string(item)
		if name == "" || ignoredPackage(name) {
			continue
		}
		dir, basename := filepath.Split(name)
		active, err := build.Default.MatchFile(dir, basename)
		if err != nil {
			return 0, err
		}
		if !active {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			return 0, err
		}
		if err := testBodies(name, source, func(test string, pos token.Position, digest string) {
			entry, exact := names[name+"\x00"+test]
			if !exact {
				entry, exact = bodies[digest]
			}
			if exact {
				blocked++
				fmt.Fprintf(os.Stderr, "%s:%d %s: %s (reviewed at %s / %s)\n", name, pos.Line, test, entry.category, entry.path, entry.name)
			}
		}); err != nil {
			return 0, err
		}
	}
	return blocked, nil
}

func ignoredPackage(name string) bool {
	parts := strings.Split(name, "/")
	for _, dir := range parts[:len(parts)-1] {
		if dir == "testdata" || dir == "vendor" || strings.HasPrefix(dir, ".") || strings.HasPrefix(dir, "_") {
			return true
		}
	}
	return false
}

func run() error {
	force := flag.Bool("required", false, "inspect the automated upstream-sync checkout")
	event := flag.String("pr-event", "", "GitHub pull_request event JSON")
	listPath := flag.String("inventory", ".github/scripts/upstream-sync-retired-tests.tsv", "reviewed excluded test functions")
	flag.Parse()
	if !*force {
		active, err := shouldInspect(*event)
		if err != nil {
			return fmt.Errorf("read PR context: %w", err)
		}
		if !active {
			fmt.Println("Upstream-sync preflight skipped for ordinary PR.")
			return nil
		}
	}
	list, err := inventory(*listPath)
	if err != nil {
		return err
	}
	blocked, err := scanTests(list)
	if err != nil {
		return err
	}
	if blocked != 0 {
		return fmt.Errorf("%d excluded test functions would enter the default build; migrate just those cases before running go test", blocked)
	}
	fmt.Printf("Upstream-sync preflight passed for %d reviewed exclusions.\n", len(list))
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "upstream-sync test preflight: %v\n", err)
		os.Exit(1)
	}
}
