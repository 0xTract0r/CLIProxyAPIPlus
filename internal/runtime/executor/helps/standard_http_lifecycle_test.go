package helps

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryStandardTransportLifecycle(t *testing.T) {
	const target = "http://transport-fixture.invalid/path"
	for _, name := range []string{"reuse", "post failure is not replayed", "request cancellation"} {
		t.Run(name, func(t *testing.T) {
			ResetProxyTransports()
			t.Cleanup(ResetProxyTransports)
			auth := &coreauth.Auth{ID: name, ProxyURL: "direct", ProxyConnectionPool: poolEnabled()}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			cfg := &config.Config{}
			client := NewProxyAwareHTTPClient(ctx, cfg, auth, time.Second)
			tr, ok := client.Transport.(*http.Transport)
			if !ok || tr.TLSHandshakeTimeout <= 0 {
				t.Fatal("explicit direct route lost its standard bounded TLS transport")
			}
			if other := NewProxyAwareHTTPClient(ctx, cfg, auth, time.Second); other.Transport != tr {
				t.Fatal("multiple clients for the same explicitly configured route lost their shared pool")
			}
			var dials atomic.Int32
			var posts atomic.Int32
			requestArrived := make(chan struct{})
			serveDone := make(chan error, 1)
			tr.DialContext = func(_ context.Context, _, _ string) (net.Conn, error) {
				if dials.Add(1) != 1 {
					return nil, errors.New("unexpected replacement connection")
				}
				c, s := net.Pipe()
				go func() {
					defer s.Close()
					r := bufio.NewReader(s)
					for i := 0; i < 2; i++ {
						req, err := http.ReadRequest(r)
						if err != nil {
							serveDone <- err
							return
						}
						_, err = io.Copy(io.Discard, req.Body)
						_ = req.Body.Close()
						if err != nil {
							serveDone <- err
							return
						}
						if req.Method == http.MethodPost {
							posts.Add(1)
						}
						if i == 1 && name != "reuse" {
							close(requestArrived)
							if name == "request cancellation" {
								_, _ = r.ReadByte()
							}
							serveDone <- nil
							return
						}
						if _, err = io.WriteString(s, "HTTP/1.1 204 No Content\r\n\r\n"); err != nil {
							serveDone <- err
							return
						}
					}
					serveDone <- nil
				}()
				return c, nil
			}
			send := func(ctx context.Context, method string) error {
				req, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader("fixture"))
				if err != nil {
					return err
				}
				resp, err := client.Do(req)
				if err == nil {
					err = resp.Body.Close()
				}
				return err
			}
			if err := send(ctx, http.MethodGet); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "reuse":
				if err := send(ctx, http.MethodGet); err != nil {
					t.Fatal(err)
				}
			case "post failure is not replayed":
				if err := send(ctx, http.MethodPost); err == nil || posts.Load() != 1 {
					t.Fatalf("POST error=%v attempts=%d", err, posts.Load())
				}
			case "request cancellation":
				finished := make(chan error, 1)
				go func() { finished <- send(ctx, http.MethodPost) }()
				select {
				case <-requestArrived:
				case <-ctx.Done():
					t.Fatal("request never reached the synthetic transport")
				}
				cancel()
				if err := <-finished; !errors.Is(err, context.Canceled) || posts.Load() != 1 {
					t.Fatalf("cancellation error=%v attempts=%d", err, posts.Load())
				}
			}
			if err := <-serveDone; err != nil {
				t.Fatal(err)
			}
			if got := dials.Load(); got != 1 {
				t.Fatalf("got %d connections, want only the original connection", got)
			}
		})
	}
}
