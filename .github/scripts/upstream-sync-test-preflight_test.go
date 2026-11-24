package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	alphaPath   = "alpha_test.go"
	betaPath    = "beta_test.go"
	alpha       = `{ t.Log("fixture alpha original") }`
	replacement = `{ t.Log("fixture alpha replacement") }`
	beta        = `{ t.Log("fixture beta original") }`
	unreviewed  = `{ t.Log("fixture changed") }`
)

func inventoryLine(path, name, original string, extra ...string) string {
	return strings.Join(append([]string{path, name, bodyHash([]byte(original)), "fixture"}, extra...), "\t") + "\n"
}

func writeInventory(t *testing.T, rows string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "review.tsv")
	if err := os.WriteFile(path, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInventorySupportsOnlyExactReviewedReplacement(t *testing.T) {
	replacementDigest := bodyHash([]byte(replacement))
	modern := inventoryLine(alphaPath, "TestAlpha", alpha, strings.ToUpper(replacementDigest))
	legacy := inventoryLine(betaPath, "TestBeta", beta)
	entries, err := inventory(writeInventory(t, "# synthetic fixtures\n"+modern+legacy))
	if err != nil || len(entries) != 2 {
		t.Fatalf("inventory() = %v, %v", entries, err)
	}
	if entries[0].replacementHash != replacementDigest || entries[1].replacementHash != "" {
		t.Fatalf("replacement/legacy parse mismatch: %v", entries)
	}

	for _, tc := range []struct{ name, rows, reason string }{
		{"empty replacement", inventoryLine(alphaPath, "TestAlpha", alpha, ""), "invalid or duplicate replacement hash"},
		{"invalid replacement", inventoryLine(alphaPath, "TestAlpha", alpha, strings.Repeat("q", 64)), "invalid or duplicate replacement hash"},
		{"wrong replacement length", inventoryLine(alphaPath, "TestAlpha", alpha, "abcd"), "invalid or duplicate replacement hash"},
		{"extra column", inventoryLine(alphaPath, "TestAlpha", alpha, replacementDigest, "extra"), "invalid inventory"},
		{"duplicate path and name", modern + inventoryLine(alphaPath, "TestAlpha", beta), "invalid or duplicate inventory"},
		{"duplicate historical body", modern + inventoryLine(betaPath, "TestBeta", alpha), "invalid or duplicate inventory"},
		{"duplicate replacement", modern + inventoryLine(betaPath, "TestBeta", beta, replacementDigest), "invalid or duplicate replacement hash"},
		{"replacement is own denied body", inventoryLine(alphaPath, "TestAlpha", alpha, bodyHash([]byte(alpha))), "is a denied body"},
		{"replacement is another denied body", legacy + inventoryLine(alphaPath, "TestAlpha", alpha, bodyHash([]byte(beta))), "is a denied body"},
		{"replacement is a later denied body", inventoryLine(alphaPath, "TestAlpha", alpha, bodyHash([]byte(beta))) + legacy, "is a denied body"},
		{"invalid path", inventoryLine("../alpha_test.go", "TestAlpha", alpha), "invalid inventory"},
		{"invalid name", inventoryLine(alphaPath, "Test Alpha", alpha), "invalid inventory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := inventory(writeInventory(t, tc.rows))
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("inventory() error = %v, want %q", err, tc.reason)
			}
		})
	}
}

func TestScanTestsHonorsReplacementAndGlobalDenial(t *testing.T) {
	rows := inventoryLine(alphaPath, "TestAlpha", alpha, bodyHash([]byte(replacement))) +
		inventoryLine(betaPath, "TestBeta", beta)
	list, err := inventory(writeInventory(t, rows))
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "--quiet")
	write := func(path, source string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{alphaPath, betaPath, "elsewhere_test.go"} {
		write(path, "package fixtures\n")
	}
	git("add", "--", alphaPath, betaPath, "elsewhere_test.go")
	for _, tc := range []struct {
		name, path, testName, body string
		blocked                    int
	}{
		{"exact reviewed replacement", alphaPath, "TestAlpha", replacement, 0},
		{"changed same-name test", alphaPath, "TestAlpha", unreviewed, 1},
		{"historical same-name test", alphaPath, "TestAlpha", alpha, 1},
		{"historical body under a different retired name", alphaPath, "TestAlpha", beta, 1},
		{"historical body moved and renamed", "elsewhere_test.go", "TestRenamed", alpha, 1},
		{"historical body moved to a helper", "elsewhere_test.go", "fixtureHelper", alpha, 1},
		{"historical body moved to a method", "elsewhere_test.go", "(fixtureReceiver) helper", alpha, 1},
		{"ordinary method shares a retired name", alphaPath, "(fixtureReceiver) TestAlpha", unreviewed, 0},
		{"four-column retired name remains closed", betaPath, "TestBeta", replacement, 1},
		{"ordinary unrelated test", "elsewhere_test.go", "TestUnrelated", unreviewed, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(tc.path, "package fixtures\nimport \"testing\"\ntype fixtureReceiver struct{}\nfunc "+tc.testName+"(t *testing.T) "+tc.body+"\n")
			t.Cleanup(func() { write(tc.path, "package fixtures\n") })
			got, err := scanTests(list)
			if err != nil || got != tc.blocked {
				t.Fatalf("scanTests() = %d, %v, want %d, nil", got, err, tc.blocked)
			}
		})
	}
}
