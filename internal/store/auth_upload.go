package store

import (
	"crypto/sha256"
	"fmt"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// authUploadRecovery is guarded by the owning store's mutex. The backend is
// authoritative: existing bootstrap restores the mirror from it after restart.
// Until convergence this process blocks other writes for the pending key.
type authUploadRecovery struct {
	pending map[string][sha256.Size]byte
}

func (r *authUploadRecovery) guard(key string) error {
	if _, pending := r.pending[key]; pending {
		return coreauth.ErrAuthUploadPayloadConflict
	}
	return nil
}

func (r *authUploadRecovery) run(key string, data []byte, install func(func() error) error, writeRemote func() error) error {
	digest := sha256.Sum256(data)
	previous, confirmed := r.pending[key]
	if confirmed && previous != digest {
		return coreauth.ErrAuthUploadPayloadConflict
	}
	commitRemote := func() error {
		if confirmed {
			return nil
		}
		if err := writeRemote(); err != nil {
			// A transport failure can leave the remote outcome unknown.
			return fmt.Errorf("%w: %w", coreauth.ErrAuthUploadRemoteUnconfirmed, err)
		}
		if r.pending == nil {
			r.pending = make(map[string][sha256.Size]byte)
		}
		r.pending[key] = digest
		confirmed = true
		return nil
	}
	if err := install(commitRemote); err != nil {
		if confirmed {
			return fmt.Errorf("%w: %w", coreauth.ErrAuthUploadLocalPending, err)
		}
		return err
	}
	if !confirmed {
		return coreauth.ErrAuthUploadRemoteUnconfirmed
	}
	delete(r.pending, key)
	return nil
}
