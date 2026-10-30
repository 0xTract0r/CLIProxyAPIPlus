package auth

// CloneForSchedulingPreview detaches metadata and the monotonic temporary-limit
// lifetime. Normal Clone intentionally shares the latter so serving snapshots
// agree on expiry; a speculative policy read must not latch expiry on live auth.
func (a *Auth) CloneForSchedulingPreview() *Auth {
	out := a.Clone()
	if out == nil {
		return nil
	}
	if a.Metadata != nil {
		out.Metadata = deepCopyMetadataValue(a.Metadata).(map[string]any)
	}
	if runtime := a.temporaryLimits; runtime != nil {
		out.temporaryLimits = &temporaryLimitRuntime{
			id: runtime.id, started: runtime.started, duration: runtime.duration,
		}
		out.temporaryLimits.expired.Store(runtime.expired.Load())
	}
	return out
}
