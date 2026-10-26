package home

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRebalanceContextDialerCancellation(t *testing.T) {
	t.Run("cancelled before dial", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		dial, cleanup := rebalanceContextDialer(ctx, func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("cancelled command attempted a dial")
			return nil, nil
		})
		defer cleanup()
		conn, err := dial(ctx, "tcp", "local")
		if conn != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("dial = %v, %v", conn, err)
		}
	})
	for _, retire := range []string{"cancel during dial", "cleanup during dial"} {
		t.Run(retire, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn, peer := net.Pipe()
			defer conn.Close()
			defer peer.Close()
			entered, release := make(chan struct{}), make(chan struct{})
			dial, cleanup := rebalanceContextDialer(ctx, func(context.Context, string, string) (net.Conn, error) {
				close(entered)
				<-release
				return conn, nil
			})
			defer cleanup()
			type dialResult struct {
				conn net.Conn
				err  error
			}
			done := make(chan dialResult, 1)
			go func() { c, err := dial(ctx, "tcp", "local"); done <- dialResult{c, err} }()
			<-entered
			if retire == "cancel during dial" {
				cancel()
			} else {
				cleanup()
			}
			close(release)
			select {
			case result := <-done:
				if result.conn != nil || !errors.Is(result.err, context.Canceled) {
					t.Fatalf("retired dial = %v, %v", result.conn, result.err)
				}
			case <-time.After(time.Second):
				t.Fatal("late dial did not return")
			}
			_ = peer.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("late connection was not closed: %v", err)
			}
		})
	}
	t.Run("cancel after dial", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		conn, peer := net.Pipe()
		defer conn.Close()
		defer peer.Close()
		dial, cleanup := rebalanceContextDialer(ctx, func(context.Context, string, string) (net.Conn, error) { return conn, nil })
		defer cleanup()
		actual, err := dial(ctx, "tcp", "local")
		if err != nil || actual != conn {
			t.Fatalf("dial changed the underlying connection: %v", err)
		}
		cancel()
		cleanup()
		_ = peer.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("cancel did not close owned connection: %v", err)
		}
	})
	t.Run("normal completion unregisters cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		conn, peer := net.Pipe()
		defer conn.Close()
		defer peer.Close()
		dial, cleanup := rebalanceContextDialer(ctx, func(context.Context, string, string) (net.Conn, error) { return conn, nil })
		defer cleanup()
		actual, err := dial(ctx, "tcp", "local")
		if err != nil || actual != conn {
			t.Fatalf("dial changed the underlying connection: %v", err)
		}
		cleanup()
		cancel()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_ = peer.SetDeadline(time.Now().Add(time.Second))
		written := make(chan error, 1)
		go func() { _, err := actual.Write([]byte("x")); written <- err }()
		buf := make([]byte, 1)
		if _, err := io.ReadFull(peer, buf); err != nil || string(buf) != "x" {
			t.Fatalf("retired cancellation closed reusable connection: %q, %v", buf, err)
		}
		if err := <-written; err != nil {
			t.Fatal(err)
		}
	})
}

func TestClusterBalanceCommandCancellationPreservesSourcePools(t *testing.T) {
	for _, mode := range []string{"parent cancellation", "normal completion"} {
		t.Run(mode, func(t *testing.T) {
			blocked, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			client, _ := newRedisCommandTestClient(t, func(args []string) string {
				if strings.EqualFold(args[0], "CLUSTER") {
					if mode == "parent cancellation" {
						close(blocked)
						<-release
					}
					return "$11\r\n{\"ok\":true}\r\n"
				}
				return balanceTestRedisReply(args)
			})
			originalCommand, originalSubscription := client.cmd, client.sub
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := client.rebalanceCommand(ctx, "", "CLUSTER", "BALANCE", "ENABLE"); done <- err }()
			if mode == "parent cancellation" {
				select {
				case <-blocked:
					cancel()
				case err := <-done:
					t.Fatalf("command ended before blocked RPC: %v", err)
				case <-time.After(time.Second):
					t.Fatal("RPC did not start")
				}
			}
			select {
			case err := <-done:
				if (err != nil) != (mode == "parent cancellation") {
					t.Fatalf("command error = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("owned RPC did not finish")
			}
			cancel()
			if client.cmd != originalCommand || client.sub != originalSubscription {
				t.Fatal("command replaced source pools")
			}
			pingCtx, stopPing := context.WithTimeout(context.Background(), time.Second)
			defer stopPing()
			if err := originalCommand.Ping(pingCtx).Err(); err != nil {
				t.Fatalf("source command pool interrupted: %v", err)
			}
			if err := originalSubscription.Ping(pingCtx).Err(); err != nil {
				t.Fatalf("source heartbeat pool interrupted: %v", err)
			}
		})
	}
}
