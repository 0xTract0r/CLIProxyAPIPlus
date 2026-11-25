package ordinaryconnect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"testing"
)

func bound(ctx context.Context, n int) context.Context {
	u, _ := url.Parse("http://127.0.0.1:18419/v1/chat/completions")
	return Bind(Allow(ctx, "fixed-grant", n), "fixed-grant", u)
}
func refusal() error { return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED} }
func TestOriginalDialBudget(t *testing.T) {
	for _, n := range []int{0, 1, 4} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			ctx := bound(context.Background(), n)
			calls := 0
			tr := &http.Transport{DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
				calls++
				if network != "tcp" || address != "127.0.0.1:18419" {
					t.Fatal("route changed")
				}
				return nil, refusal()
			}}
			Wrap(ctx, tr)
			_, err := tr.DialContext(ctx, "tcp", "127.0.0.1:18419")
			if calls != n+1 || !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatalf("calls=%d want=%d", calls, n+1)
			}
		})
	}
}
func TestOriginalDialSuccessAfterRefusal(t *testing.T) {
	ctx := bound(context.Background(), 1)
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	calls := 0
	tr := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		calls++
		if calls == 1 {
			return nil, refusal()
		}
		return a, nil
	}}
	Wrap(ctx, tr)
	c, err := tr.DialContext(ctx, "tcp", "127.0.0.1:18419")
	if err != nil || c != a || calls != 2 {
		t.Fatal("original success not preserved")
	}
}
func TestOriginalCustomVetoPreserved(t *testing.T) {
	for _, veto := range []error{errors.New("caller veto"), fmt.Errorf("caller veto: %w", refusal())} {
		ctx := bound(context.Background(), 4)
		calls := 0
		tr := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { calls++; return nil, veto }}
		Wrap(ctx, tr)
		_, err := tr.DialContext(ctx, "tcp", "127.0.0.1:18419")
		if err != veto || calls != 1 {
			t.Fatal("custom veto intercepted")
		}
	}
}
func TestDetachedDialUsesRequestCancellation(t *testing.T) {
	request, cancel := context.WithCancel(context.Background())
	ctx := bound(request, 4)
	calls := 0
	tr := &http.Transport{DialContext: func(call context.Context, _, _ string) (net.Conn, error) {
		calls++
		cancel()
		<-call.Done()
		return nil, refusal()
	}}
	Wrap(ctx, tr)
	detached := context.WithoutCancel(ctx)
	_, err := tr.DialContext(detached, "tcp", "127.0.0.1:18419")
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatal("detached dial retried after request cancellation")
	}
}
func TestNonNilConnectionOwnershipPreserved(t *testing.T) {
	request, cancel := context.WithCancel(context.Background())
	ctx := bound(request, 4)
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	veto := refusal()
	tr := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { cancel(); return a, veto }}
	Wrap(ctx, tr)
	c, err := tr.DialContext(context.WithoutCancel(ctx), "tcp", "127.0.0.1:18419")
	if c != a || err != veto {
		t.Fatal("connection ownership changed")
	}
}

func TestNonTCPVetoUntouched(t *testing.T) {
	ctx := bound(context.Background(), 4)
	calls := 0
	original := refusal()
	tr := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { calls++; return nil, original }}
	Wrap(ctx, tr)
	_, err := tr.DialContext(ctx, "udp", "127.0.0.1:18419")
	if calls != 1 || err != original {
		t.Fatal("non-TCP intercepted")
	}
}
func TestActualKernelRefusalUsesSameOriginalFunction(t *testing.T) {
	// Reserve an ephemeral loopback port so the test does not depend on a system service.
	reservation, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	u := &url.URL{Scheme: "http", Host: address, Path: "/v1/chat/completions"}
	ctx := Bind(Allow(context.Background(), "fixed-grant", 1), "fixed-grant", u)
	calls := 0
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	dialer := &net.Dialer{}
	tr := &http.Transport{DialContext: func(call context.Context, network, addr string) (net.Conn, error) {
		calls++
		if network != "tcp" || addr != address {
			t.Fatal("target changed")
		}
		if calls == 1 {
			c, e := dialer.DialContext(call, network, addr)
			if !errors.Is(e, syscall.ECONNREFUSED) {
				t.Fatalf("kernel refusal missing: %v", e)
			}
			return c, e
		}
		return a, nil
	}}
	Wrap(ctx, tr)
	c, err := tr.DialContext(ctx, "tcp", address)
	if calls != 2 || c != a || err != nil {
		t.Fatal("same original callback not reentered")
	}
}

func TestExhaustedTracksOnlyConsumedRetryBudget(t *testing.T) {
	for _, n := range []int{0, 1, 3} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			ctx := bound(context.Background(), n)
			calls := 0
			original := refusal()
			tr := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { calls++; return nil, original }}
			Wrap(ctx, tr)
			_, err := tr.DialContext(ctx, "tcp", "127.0.0.1:18419")
			if calls != n+1 || Exhausted(ctx) != (n > 0) || err != original || !errors.Is(err, syscall.ECONNREFUSED) {
				t.Error("budget flag or original error changed")
			}
		})
	}
	ctx := bound(context.Background(), 3)
	veto := errors.New("custom host veto")
	tr := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return nil, veto }}
	Wrap(ctx, tr)
	_, err := tr.DialContext(ctx, "tcp", "127.0.0.1:18419")
	if Exhausted(ctx) || err != veto {
		t.Error("unconsumed veto changed")
	}
}
func TestTypedNilOpErrorIsReturnedUntouched(t *testing.T) {
	ctx := bound(context.Background(), 3)
	calls := 0
	var typedNil *net.OpError
	tr := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { calls++; return nil, typedNil }}
	Wrap(ctx, tr)
	conn, err := tr.DialContext(ctx, "tcp", "127.0.0.1:18419")
	if conn != nil || err != error(typedNil) || calls != 1 || Exhausted(ctx) {
		t.Error("typed nil was changed or retried")
	}
}

func TestRecoveredDialAndOtherVetoDoNotExhaust(t *testing.T) {
	t.Run("second_dial_success_then_http500", func(t *testing.T) {
		ctx := bound(context.Background(), 1)
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		calls := 0
		go func() {
			defer b.Close()
			r, err := http.ReadRequest(bufio.NewReader(b))
			if err != nil {
				return
			}
			r.Body.Close()
			fmt.Fprint(b, "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
		}()
		tr := &http.Transport{DisableKeepAlives: true, DialContext: func(context.Context, string, string) (net.Conn, error) {
			calls++
			if calls == 1 {
				return nil, refusal()
			}
			return a, nil
		}}
		Wrap(ctx, tr)
		defer tr.CloseIdleConnections()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:18419/v1/chat/completions", nil)
		response, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil {
			t.Fatal("in-memory ordinary HTTP failed")
		}
		defer response.Body.Close()
		if calls != 2 || response.StatusCode != 500 || Exhausted(ctx) {
			t.Error("successful connect/HTTP500 marked exhausted")
		}
	})
	t.Run("other_veto_after_one_extra_call", func(t *testing.T) {
		ctx := bound(context.Background(), 1)
		calls := 0
		veto := errors.New("original dial veto after refusal")
		tr := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			calls++
			if calls == 1 {
				return nil, refusal()
			}
			return nil, veto
		}}
		Wrap(ctx, tr)
		_, err := tr.DialContext(ctx, "tcp", "127.0.0.1:18419")
		if calls != 2 || err != veto || Exhausted(ctx) {
			t.Error("other veto marked exhausted or changed")
		}
	})
}
