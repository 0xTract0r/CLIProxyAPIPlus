package api

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"strings"
)

type ampProtectedTransport struct {
	http.RoundTripper
	caller      string
	upstreamKey string
}

func (t ampProtectedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// ReverseProxy forwards informational headers before ModifyResponse.
	// The transport hook runs before the proxy's existing 1xx hook.
	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(_ int, header textproto.MIMEHeader) error {
			redactAmpForwardHeaders(http.Header(header), t.caller)
			redactAmpForwardHeaders(http.Header(header), t.upstreamKey)
			return nil
		},
	}
	return t.RoundTripper.RoundTrip(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
}

func protectAmpForwardResponse(resp *http.Response, caller, upstreamKey string) error {
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return errors.New("Amp upstream protocol upgrade is unsupported with credential forwarding")
	}
	encodings := resp.Header.Values("Content-Encoding")
	for _, value := range encodings {
		encoding := strings.TrimSpace(value)
		if encoding == "" || strings.EqualFold(encoding, "identity") {
			continue
		}
		if len(encodings) != 1 || !strings.EqualFold(encoding, "gzip") {
			return errors.New("unexpected Amp upstream content encoding")
		}
		if resp.Body != nil && resp.Body != http.NoBody {
			decoded, err := gzip.NewReader(resp.Body)
			if err != nil {
				return errors.New("invalid Amp upstream gzip body")
			}
			resp.Body = &ampForwardGzipBody{Reader: decoded, original: resp.Body}
			resp.ContentLength = -1
			resp.Header.Del("Content-Encoding")
			resp.Header.Del("Content-Length")
		}
	}
	for _, secret := range []string{caller, upstreamKey} {
		redactAmpForwardHeaders(resp.Header, secret)
		redactAmpForwardHeaders(resp.Trailer, secret)
	}
	if resp.Body != nil && resp.Body != http.NoBody {
		// Read may populate trailers after ModifyResponse returns.
		body := resp.Body
		for _, secret := range []string{caller, upstreamKey} {
			body = &ampForwardRedactedBody{
				ReadCloser: body,
				secret:     []byte(secret),
				onEOF: func() {
					redactAmpForwardHeaders(resp.Trailer, secret)
				},
			}
		}
		resp.Body = body
	}
	return nil
}

type ampForwardGzipBody struct {
	*gzip.Reader
	original io.ReadCloser
}

func (b *ampForwardGzipBody) Close() error {
	return errors.Join(b.Reader.Close(), b.original.Close())
}

func redactAmpForwardHeaders(headers http.Header, secret string) {
	for _, name := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "X-Goog-Api-Key", "Set-Cookie"} {
		headers.Del(name)
	}
	for name, values := range headers {
		if strings.Contains(name, secret) {
			delete(headers, name)
			continue
		}
		for i, value := range values {
			values[i] = strings.ReplaceAll(value, secret, strings.Repeat("*", len(secret)))
		}
	}
}

// Keep only a suffix that could be a split key. Other bytes can be sent
// immediately, preserving streaming and Content-Length.
type ampForwardRedactedBody struct {
	io.ReadCloser
	secret  []byte
	pending []byte
	ready   []byte
	err     error
	onEOF   func()
}

func (b *ampForwardRedactedBody) Read(out []byte) (int, error) {
	if len(out) == 0 {
		return 0, nil
	}
	if len(b.ready) == 0 && b.err == nil {
		var chunk [4096]byte
		n, err := b.ReadCloser.Read(chunk[:])
		b.err = err
		input := append(b.pending, chunk[:n]...)
		b.pending = nil
		replacement := bytes.Repeat([]byte{'*'}, len(b.secret))
		for {
			index := bytes.Index(input, b.secret)
			if index == -1 {
				break
			}
			b.ready = append(b.ready, input[:index]...)
			b.ready = append(b.ready, replacement...)
			input = input[index+len(b.secret):]
		}
		if err == nil {
			for keep := min(len(input), len(b.secret)-1); keep > 0; keep-- {
				if bytes.Equal(input[len(input)-keep:], b.secret[:keep]) {
					b.pending = append(b.pending, input[len(input)-keep:]...)
					input = input[:len(input)-keep]
					break
				}
			}
		} else {
			b.onEOF()
		}
		b.ready = append(b.ready, input...)
	}
	if len(b.ready) != 0 {
		n := copy(out, b.ready)
		b.ready = b.ready[n:]
		return n, nil
	}
	return 0, b.err
}
