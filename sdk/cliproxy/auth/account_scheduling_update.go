package auth

import (
	"context"
	"errors"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

var ErrAccountSchedulingNotFound = errors.New("account scheduling target not found")
var ErrAccountSchedulingPersist = errors.New("account scheduling persistence failed")

type accountPolicyWriteKey struct{}

// UpdateAccountScheduling serializes read/merge/save/publication with live
// account updates. A failed save never publishes part of an operator patch.
func (m *Manager) UpdateAccountScheduling(ctx context.Context, id, index string, apply func(*Auth, internalconfig.AccountSchedulingConfig) error) (*Auth, error) {
	m.pacingMu.Lock()
	m.mu.Lock()
	a := m.auths[id]
	if a == nil || (index != "" && a.Index != index) {
		m.mu.Unlock()
		m.pacingMu.Unlock()
		return nil, ErrAccountSchedulingNotFound
	}
	updated := a.Clone()
	if err := apply(updated, m.accountSchedulingConfig()); err != nil {
		m.mu.Unlock()
		m.pacingMu.Unlock()
		return nil, err
	}
	updated.UpdatedAt = time.Now()
	if err := m.persist(context.WithValue(ctx, accountPolicyWriteKey{}, true), updated); err != nil {
		m.mu.Unlock()
		m.pacingMu.Unlock()
		return nil, ErrAccountSchedulingPersist
	}
	m.auths[id] = updated.Clone()
	m.mu.Unlock()
	m.refreshWarmupPacingLocked()
	m.pacingMu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(updated)
	}
	m.hook.OnAuthUpdated(ctx, updated.Clone())
	notifyPacingAdmission()
	return updated.Clone(), nil
}
