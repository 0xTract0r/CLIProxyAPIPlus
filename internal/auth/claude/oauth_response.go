package claude

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/lzw"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func readClaudeOAuthResponseBody(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("read Claude OAuth response: body is nil")
	}
	encoded, errRead := readBoundedClaudeOAuthBody(resp.Body)
	if errRead != nil {
		return nil, errRead
	}
	encodings := strings.Split(strings.Join(resp.Header.Values("Content-Encoding"), ","), ",")
	for index := len(encodings) - 1; index >= 0; index-- {
		encoding := strings.ToLower(strings.TrimSpace(encodings[index]))
		if encoding == "" || encoding == "identity" {
			continue
		}
		var errDecode error
		encoded, errDecode = decodeClaudeOAuthEncoding(encoded, encoding)
		if errDecode != nil {
			return nil, errDecode
		}
	}
	return encoded, nil
}

func decodeClaudeOAuthEncoding(encoded []byte, encoding string) ([]byte, error) {
	var reader io.ReadCloser
	switch encoding {
	case "gzip", "x-gzip":
		gzipReader, errGzip := gzip.NewReader(bytes.NewReader(encoded))
		if errGzip != nil {
			return nil, fmt.Errorf("decode Claude OAuth gzip response: %w", errGzip)
		}
		reader = gzipReader
	case "deflate":
		zlibReader, errZlib := zlib.NewReader(bytes.NewReader(encoded))
		if errZlib == nil {
			reader = zlibReader
		} else {
			reader = flate.NewReader(bytes.NewReader(encoded))
		}
	case "br":
		reader = io.NopCloser(brotli.NewReader(bytes.NewReader(encoded)))
	case "zstd":
		decoder, err := zstd.NewReader(bytes.NewReader(encoded), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(8<<20), zstd.WithDecoderMaxWindow(4<<20))
		if err != nil {
			return nil, fmt.Errorf("failed to initialize Claude OAuth zstd decoder")
		}
		reader = decoder.IOReadCloser()
	case "compress":
		reader = lzw.NewReader(bytes.NewReader(encoded), lzw.MSB, 8)
	default:
		return nil, fmt.Errorf("decode Claude OAuth response: unsupported content encoding")
	}
	decoded, errDecoded := readBoundedClaudeOAuthBody(reader)
	if errDecoded != nil {
		_ = reader.Close()
		return nil, fmt.Errorf("decode Claude OAuth %s response: %w", encoding, errDecoded)
	}
	if errClose := reader.Close(); errClose != nil {
		return nil, fmt.Errorf("close Claude OAuth %s decoder: %w", encoding, errClose)
	}
	return decoded, nil
}

const maxClaudeOAuthBodyBytes = 4 << 20

func readBoundedClaudeOAuthBody(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxClaudeOAuthBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read Claude OAuth response")
	}
	if len(body) > maxClaudeOAuthBodyBytes {
		return nil, fmt.Errorf("Claude OAuth response exceeds size limit")
	}
	return body, nil
}
