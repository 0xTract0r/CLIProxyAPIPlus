package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOrdinaryFileStoreEquivalentNumbersDoNotRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "equivalent.json")
	raw := []byte(` {"type":"test", "disabled":false,"n":1.00,"nested":[-0,100e-2],"sequence":9007199254740993} `)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1234567000, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	s := NewFileTokenStore().WithBaseDir(dir)
	records, err := s.List(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatalf("list: %v/%d", err, len(records))
	}
	records[0].Metadata["n"] = json.Number("1e0")
	records[0].Metadata["nested"] = []any{json.Number("0.0"), json.Number("1")}
	for i := 0; i < 2; i++ {
		if _, err = s.Save(context.Background(), records[0]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("equivalent JSON rewritten: %s / %v", got, err)
	}
	st, err := os.Stat(path)
	if err != nil || !st.ModTime().Equal(stamp) || st.Mode().Perm() != 0600 {
		t.Fatalf("stat changed: %v / %v", st, err)
	}
}
