package auth

import "context"

type fileSnapshotReaderKey struct{}

// WithFileSnapshotReader validates a prepared file registration with the
// caller's already scoped file reader. The expected snapshot digest remains
// enforced by the manager.
func WithFileSnapshotReader(ctx context.Context, read func(string) ([]byte, error)) context.Context {
	return context.WithValue(ctx, fileSnapshotReaderKey{}, read)
}

func (a *Auth) fileSnapshotMatchesContext(ctx context.Context) bool {
	if !a.HasFileSnapshot() {
		return true
	}
	if ctx != nil {
		if read, ok := ctx.Value(fileSnapshotReaderKey{}).(func(string) ([]byte, error)); ok {
			data, err := read(a.FileSnapshotPath())
			return err == nil && len(data) != 0 && canonicalAuthFileDigest(data) == a.fileSnapshotDigest
		}
	}
	return a.FileSnapshotMatchesDisk()
}
