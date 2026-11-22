package auth

import (
	"context"
	"errors"
)

// ErrAuthPublicationOutOfScope indicates that a committed file is no longer
// eligible to be published in this runtime.
var ErrAuthPublicationOutOfScope = errors.New("auth publication is outside the current file scope")

// AuthPublicationFence admits a short in-memory publication. The returned
// release function must run before I/O or external callbacks.
type AuthPublicationFence struct {
	BeginPublication func() (release func(), eligible bool)
}

func (f *AuthPublicationFence) Begin() (func(), bool) {
	if f == nil {
		return func() {}, true
	}
	return f.BeginPublication()
}

type authPublicationFenceKey struct{}

func WithAuthPublicationFence(ctx context.Context, fence *AuthPublicationFence) context.Context {
	if fence == nil {
		return ctx
	}
	return context.WithValue(ctx, authPublicationFenceKey{}, fence)
}

func authPublicationFence(ctx context.Context) *AuthPublicationFence {
	if ctx == nil {
		return nil
	}
	fence, _ := ctx.Value(authPublicationFenceKey{}).(*AuthPublicationFence)
	return fence
}
