package auth

import (
	"context"
	"strings"
)

// This file carries the fork-only Manager device high-water raise methods,
// ported out of the fork conductor monolith into the upstream split-file
// structure. Executors call these on every serving path to persist a
// monotonically increasing device version high-water (anti-correlation: keeps
// the outbound device version from falling back to the floor across restarts).

// RaiseClaudeDeviceHighWater raises the persisted Claude device high-water for
// authID to profile, only on a strict version increase (steady-state zero disk
// writes). Returns whether it raised and any persistence error.
func (m *Manager) RaiseClaudeDeviceHighWater(ctx context.Context, authID string, profile ClaudeDeviceHighWater) (bool, error) {
	if m == nil {
		return false, nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" || !profile.valid() {
		return false, nil
	}
	incomingVersion, ok := profile.parsedVersion()
	if !ok {
		return false, nil
	}

	m.mu.Lock()
	existing, ok := m.auths[authID]
	if !ok || existing == nil {
		m.mu.Unlock()
		return false, nil
	}

	// Compare the incoming version against the currently persisted high-water and
	// only raise on a strict increase (天然防抖：稳态零写盘).
	if current, hasCurrent := ClaudeDeviceHighWaterFromMetadata(existing.Metadata); hasCurrent {
		if currentVersion, currentOK := current.parsedVersion(); currentOK {
			if incomingVersion.compare(currentVersion) <= 0 {
				m.mu.Unlock()
				return false, nil
			}
		}
	}

	if existing.Metadata == nil {
		existing.Metadata = make(map[string]any)
	}
	// Whole-map replacement: assign a brand-new map so a subsequent Clone never
	// shares this nested map with the live auth.
	existing.Metadata[ClaudeDeviceHighWaterMetadataKey] = claudeDeviceHighWaterToMetadataMap(profile)
	snapshot := existing.Clone()
	m.mu.Unlock()

	if snapshot == nil {
		return true, nil
	}
	// Persist outside the lock: surface the error so callers can log it, but the
	// in-memory high-water is already raised.
	if err := m.persist(ctx, snapshot); err != nil {
		return true, err
	}
	return true, nil
}

// RaiseCodexDeviceHighWater raises the persisted Codex device high-water for
// authID to profile, only on a strict version increase.
func (m *Manager) RaiseCodexDeviceHighWater(ctx context.Context, authID string, profile CodexDeviceHighWater) (bool, error) {
	if m == nil {
		return false, nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" || !profile.valid() {
		return false, nil
	}
	incomingVersion, ok := profile.parsedVersion()
	if !ok {
		return false, nil
	}

	m.mu.Lock()
	existing, ok := m.auths[authID]
	if !ok || existing == nil {
		m.mu.Unlock()
		return false, nil
	}

	// Compare the incoming version against the currently persisted high-water and
	// only raise on a strict increase (天然防抖：稳态零写盘).
	if current, hasCurrent := CodexDeviceHighWaterFromMetadata(existing.Metadata); hasCurrent {
		if currentVersion, currentOK := current.parsedVersion(); currentOK {
			if incomingVersion.compare(currentVersion) <= 0 {
				m.mu.Unlock()
				return false, nil
			}
		}
	}

	if existing.Metadata == nil {
		existing.Metadata = make(map[string]any)
	}
	// Whole-map replacement: assign a brand-new map so a subsequent Clone never
	// shares this nested map with the live auth.
	existing.Metadata[CodexDeviceHighWaterMetadataKey] = codexDeviceHighWaterToMetadataMap(profile)
	snapshot := existing.Clone()
	m.mu.Unlock()

	if snapshot == nil {
		return true, nil
	}
	// Persist outside the lock: surface the error so callers can log it, but the
	// in-memory high-water is already raised.
	if err := m.persist(ctx, snapshot); err != nil {
		return true, err
	}
	return true, nil
}
