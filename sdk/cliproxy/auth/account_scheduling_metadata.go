package auth

import "strings"

// AccountSchedulingMetadataKey is the single TOP-LEVEL auth.Metadata key that
// namespaces every adaptive account-scheduling field
// (openspec/changes/add-adaptive-account-scheduling design §8.5, spec.md
// "账号调度 metadata 与投影命名空间统一"). Its value is a nested object holding
// the scheduling sub-keys:
//
//	account_scheduling: { tier_override, first_production_at, rate_scale, tier_source }
//
// Why one namespaced object instead of the earlier bare top-level keys
// (tier_override / first_production_at): those sat in the same flat top-level
// space as the farm feature's farm_* keys (collision risk) and shared no word
// root with the config section (account-scheduling) or the management projection.
// Collapsing them under one object removes both problems.
//
// Why STILL top-level (not nested under quota_snapshot): the ~45min quota refresh
// replaces the nested quota_snapshot object wholesale, but Auth.Clone (types.go)
// copies every TOP-LEVEL Metadata key through untouched (see quota_snapshots.go),
// so a top-level account_scheduling object -- and every sub-key in it -- survives
// a quota refresh, whereas anything placed inside quota_snapshot would be wiped
// (design §6.4).
const AccountSchedulingMetadataKey = "account_scheduling"

// accountSchedulingRateScaleKey / accountSchedulingTierSourceKey are the
// scheduling sub-keys introduced with the §8.5 namespace unification; unlike
// tier_override / first_production_at (TierOverrideMetadataKey /
// FirstProductionAtMetadataKey, which also have a legacy bare top-level form
// honored on read) these two have no legacy bare form -- they only ever live
// inside the account_scheduling object.
const (
	accountSchedulingRateScaleKey  = "rate_scale"
	accountSchedulingTierSourceKey = "tier_source"
)

// accountSchedulingObject returns the parsed account_scheduling sub-object from
// meta. The result is READ-ONLY: for a nested value stored as anything other than
// a map[string]any (e.g. a map[string]string, or a shape rebuilt from JSON) it is
// a copy, so callers must write through setAccountSchedulingValue, never by
// mutating the returned map.
func accountSchedulingObject(meta map[string]any) (map[string]any, bool) {
	if len(meta) == 0 {
		return nil, false
	}
	raw, ok := meta[AccountSchedulingMetadataKey]
	if !ok {
		return nil, false
	}
	return metadataObject(raw)
}

// accountSchedulingRawValue returns the raw value for a scheduling sub-key,
// preferring the namespaced account_scheduling object over the legacy bare
// top-level key (dual-read migration, design §8.5 / spec.md "老裸键 dual-read
// 迁移"). ok reports whether a value was found in EITHER location; the caller
// parses/validates it.
func accountSchedulingRawValue(meta map[string]any, key string) (any, bool) {
	if len(meta) == 0 {
		return nil, false
	}
	if obj, ok := accountSchedulingObject(meta); ok {
		if raw, ok := obj[key]; ok {
			return raw, true
		}
	}
	if raw, ok := meta[key]; ok {
		return raw, true
	}
	return nil, false
}

// accountSchedulingString reads a scheduling sub-key as a normalized (lowercased,
// whitespace-trimmed) string, preferring the namespaced object over the legacy
// bare key. Returns "" when neither location holds a non-empty string.
func accountSchedulingString(meta map[string]any, key string) string {
	if len(meta) == 0 {
		return ""
	}
	if obj, ok := accountSchedulingObject(meta); ok {
		if s := normalizedMetadataString(obj[key]); s != "" {
			return s
		}
	}
	return normalizedMetadataString(meta[key])
}

// normalizedMetadataString lowercases and whitespace-trims a string metadata
// value, returning "" for any non-string or blank value.
func normalizedMetadataString(raw any) string {
	s, ok := raw.(string)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// setAccountSchedulingValue writes value at key inside the top-level
// account_scheduling object, creating the object when absent and preserving every
// other sub-key. It always writes to a map[string]any that is wired back into
// meta (never a detached copy): if the existing value is some other shape it is
// materialized into a fresh map[string]any, carrying its sub-keys over, and
// reattached -- so the write is guaranteed to persist. meta MUST be non-nil
// (callers ensure this before calling).
func setAccountSchedulingValue(meta map[string]any, key string, value any) {
	obj, isMap := meta[AccountSchedulingMetadataKey].(map[string]any)
	if !isMap {
		obj = map[string]any{}
		if existing, ok := metadataObject(meta[AccountSchedulingMetadataKey]); ok {
			for k, v := range existing {
				obj[k] = v
			}
		}
		meta[AccountSchedulingMetadataKey] = obj
	}
	obj[key] = value
}

// clearAccountSchedulingValue removes key from BOTH the namespaced
// account_scheduling object and the legacy bare top-level key. Clearing both is
// mandatory: accountSchedulingRawValue / accountSchedulingString dual-read the
// legacy bare key as a fallback (design §8.5 / spec.md "老裸键 dual-read 迁移"),
// so deleting only the namespaced sub-key would let a stale bare value resurface
// on the next read. A non-map[string]any object shape (e.g. rebuilt from JSON as
// map[string]string) is materialized into a writable map[string]any so the delete
// persists, mirroring setAccountSchedulingValue. When the namespaced object
// becomes empty it is dropped entirely so no empty "{}" lingers in the persisted
// metadata. Safe on an empty/absent meta.
func clearAccountSchedulingValue(meta map[string]any, key string) {
	if len(meta) == 0 {
		return
	}
	if raw, ok := meta[AccountSchedulingMetadataKey]; ok {
		obj, isMap := raw.(map[string]any)
		if !isMap {
			if existing, ok := metadataObject(raw); ok {
				obj = make(map[string]any, len(existing))
				for k, v := range existing {
					obj[k] = v
				}
			}
		}
		if obj != nil {
			delete(obj, key)
			if len(obj) == 0 {
				delete(meta, AccountSchedulingMetadataKey)
			} else {
				meta[AccountSchedulingMetadataKey] = obj
			}
		}
	}
	delete(meta, key)
}
