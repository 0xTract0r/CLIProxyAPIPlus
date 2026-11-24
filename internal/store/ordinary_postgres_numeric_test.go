package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"io"
	"testing"
	"time"
)

// Only the SQL row transport is synthetic; List executes the real production reader.
type ordinaryNumericDriver struct{ payloads []string }
type ordinaryNumericConn struct{ payloads []string }
type ordinaryNumericRows struct {
	payloads []string
	index    int
}

func (d *ordinaryNumericDriver) Open(string) (driver.Conn, error) {
	return &ordinaryNumericConn{d.payloads}, nil
}
func (*ordinaryNumericConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not supported")
}
func (*ordinaryNumericConn) Close() error              { return nil }
func (*ordinaryNumericConn) Begin() (driver.Tx, error) { return nil, errors.New("not supported") }
func (c *ordinaryNumericConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &ordinaryNumericRows{payloads: c.payloads}, nil
}
func (*ordinaryNumericRows) Columns() []string {
	return []string{"id", "content", "created_at", "updated_at"}
}
func (*ordinaryNumericRows) Close() error { return nil }
func (r *ordinaryNumericRows) Next(v []driver.Value) error {
	if r.index >= len(r.payloads) {
		return io.EOF
	}
	v[0] = "ordinary.json"
	v[1] = r.payloads[r.index]
	v[2] = time.Unix(1, 0)
	v[3] = time.Unix(2, 0)
	r.index++
	return nil
}
func TestOrdinaryPostgresListPreservesNumbersAndRejectsTrailing(t *testing.T) {
	const raw = `{"type":"test","disabled":false,"sequence":9007199254740993,"nested":[9007199254740997,1.00]}`
	sql.Register("ordinary_numeric_readback", &ordinaryNumericDriver{[]string{raw, raw + " {}", raw + "junk"}})
	db, err := sql.Open("ordinary_numeric_readback", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &PostgresStore{db: db, authDir: t.TempDir(), cfg: PostgresStoreConfig{AuthTable: "fixture"}}
	records, err := s.List(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatalf("List %d / %v", len(records), err)
	}
	got, err := json.Marshal(records[0].Metadata)
	if err != nil || !coreauth.EqualCredentialJSON(got, []byte(raw)) {
		t.Fatalf("numeric row changed %s / %v", got, err)
	}
}
