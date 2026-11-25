package requestlogfixture

import (
	"compress/gzip"
	"io"
	"os"
)

// Read reads the entire gzip stream, including its checksum.
func Read(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reader, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}
