package auth

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// RemoveDurable excludes store reloads even when no IDs are registered.
// It commits storage deletion under the sorted ID and persist locks.
// Callers deleting a source file must reserve the path before enumerating IDs.
// The commit callback must not mutate records protected by those gates.
func (m *Manager) RemoveDurable(ctx context.Context, ids []string, commit func() error) error {
	if commit == nil {
		return fmt.Errorf("missing durable removal operation")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ids = normalizeRemoveIDs(ids)
	if m == nil {
		return commit()
	}
	release, err := m.lockAuthMutationsContext(ctx, ids)
	if err != nil {
		return err
	}
	var afters []func()
	defer func() {
		for _, after := range afters {
			after()
		}
	}()
	defer release()

	locks := make([]*authPersistLock, 0, len(ids))
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].unlock()
		}
	}()
	for _, id := range ids {
		lock := m.persistenceLock(id)
		if err := lock.lock(ctx); err != nil {
			return err
		}
		locks = append(locks, lock)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := commit(); err != nil {
		return err
	}
	afters = make([]func(), 0, len(ids))
	m.mu.Lock()
	for i, id := range ids {
		epoch, after := m.retireAuthLocked(ctx, id)
		locks[i].markRetired(epoch)
		if after != nil {
			afters = append(afters, after)
		}
	}
	m.mu.Unlock()
	return nil
}

func normalizeRemoveIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out) // 多门固定顺序，防多删除者互锁
	return out
}
