package logging

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type countingLimitWriter struct {
	data  bytes.Buffer
	limit int
}

func (w *countingLimitWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.data.Len()
	if remaining >= len(p) {
		return w.data.Write(p)
	}
	n, _ := w.data.Write(p[:remaining])
	return n, io.ErrShortWrite
}

func TestTimelineWriteToReportsCommittedBytes(t *testing.T) {
	var _ io.WriterTo = (*FileBodySource)(nil)
	s, err := NewFileBodySourceInDir(t.TempDir(), "counts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Cleanup(); err != nil {
			t.Error(err)
		}
	})
	if err := s.AppendTimelinePart([]byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendPart([]byte("two")); err != nil {
		t.Fatal(err)
	}
	const want = "one\n\ntwo\n"
	for limit := 0; limit <= len(want); limit++ {
		out := countingLimitWriter{limit: limit}
		count, err := s.WriteTo(&out)
		if count != int64(limit) || out.data.String() != want[:limit] {
			t.Fatalf("limit %d returned %d, %q", limit, count, out.data.String())
		}
		if limit < len(want) && !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("limit %d error: %v", limit, err)
		}
		if limit == len(want) && err != nil {
			t.Fatal(err)
		}
	}
}
