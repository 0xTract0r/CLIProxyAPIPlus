package ordinaryconnect

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
)

type key struct{}
type permit struct {
	auth      string
	budget    int
	exhausted atomic.Bool
}
type peer struct {
	request context.Context
	address string
	budget  int
	first   atomic.Bool
	permit  *permit
}
type peerKey struct{}

func Allow(ctx context.Context, auth string, budget int) context.Context {
	if budget <= 0 {
		return ctx
	}
	return context.WithValue(ctx, key{}, &permit{auth: auth, budget: budget})
}
func Bind(ctx context.Context, auth string, u *url.URL) context.Context {
	p, ok := ctx.Value(key{}).(*permit)
	if !ok || p == nil || p.auth != auth || u == nil || u.User != nil {
		return ctx
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else if u.Scheme == "https" {
			port = "443"
		} else {
			return ctx
		}
	}
	return context.WithValue(ctx, peerKey{}, &peer{request: ctx, address: net.JoinHostPort(u.Hostname(), port), budget: p.budget, permit: p})
}

// Wrap only decorates a fresh direct transport. It never supplies a new dialer.
func Wrap(ctx context.Context, tr *http.Transport) {
	p, _ := ctx.Value(peerKey{}).(*peer)
	if p == nil || tr == nil || tr.DialContext == nil || tr.Proxy != nil || tr.Dial != nil || tr.DialTLS != nil || tr.DialTLSContext != nil {
		return
	}
	original := tr.DialContext
	tr.DialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		bound, _ := dialCtx.Value(peerKey{}).(*peer)
		if (network != "tcp" && network != "tcp4" && network != "tcp6") || bound != p || !strings.EqualFold(address, p.address) || !p.first.CompareAndSwap(false, true) {
			return original(dialCtx, network, address)
		}
		// net/http may detach dialing from request cancellation. Preserve the dial
		// context values/deadline AND bind cancellation to the actual request.
		callCtx, cancel := context.WithCancel(dialCtx)
		stop := context.AfterFunc(p.request, cancel)
		defer stop()
		defer cancel()
		for attempt := 0; ; attempt++ {
			if err := p.request.Err(); err != nil {
				return nil, err
			}
			if err := callCtx.Err(); err != nil {
				return nil, err
			}
			conn, err := original(callCtx, network, address)
			op, standard := err.(*net.OpError)
			if conn != nil || !standard || op == nil || op.Op != "dial" || !(op.Net == "tcp" || op.Net == "tcp4" || op.Net == "tcp6") || !errors.Is(op.Err, syscall.ECONNREFUSED) {
				return conn, err
			}
			if p.request.Err() != nil {
				return nil, p.request.Err()
			}
			if callCtx.Err() != nil {
				return nil, callCtx.Err()
			}
			if attempt >= p.budget {
				// All additional calls were spent and the final original dial
				// still definitively refused before any connection existed.
				if attempt > 0 {
					p.permit.exhausted.Store(true)
				}
				return nil, err
			}
		}
	}
}

// Exhausted reports only a positive retry budget fully spent on the same
// definitive refused-dial shape, with no connection and no observed cancellation.
// Success, later HTTP errors, custom vetoes and zero budget do not set it.
func Exhausted(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	p, _ := ctx.Value(key{}).(*permit)
	return p != nil && p.exhausted.Load()
}
