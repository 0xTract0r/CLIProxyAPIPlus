package auth

// FarmEnrolledMetadataKey is the persisted auth.Metadata key for the
// account-level device-farm enrollment flag (telemetry-device-farm TR1).
// It is the single source of truth for whether an operator has explicitly
// enrolled this account into the farm, independent of whether the account is
// currently bound to a real container. Binding state is a separate concept
// tracked by ClaudeDeviceIDSource/farm_bound (provisioned_gate.go): an
// account can be enrolled but not yet bound (pending provisioning), or
// bound while enrollment metadata is absent on legacy records.
const FarmEnrolledMetadataKey = "farm_enrolled"

// AuthFarmEnrolled reports whether the given auth is marked farm-enrolled.
// It reads Metadata[FarmEnrolledMetadataKey] and normalizes the stored value
// the same way the package's other Metadata-derived booleans do
// (parseBoolAny accepts bool, numeric, and string encodings such as
// "true"/"1"). A nil auth, empty metadata, missing key, or a value that does
// not parse as a bool is treated as not-enrolled (false); this keeps the
// default fail-closed for brand-new and legacy records that predate this
// field, and callers never need to special-case "key absent" separately from
// "explicitly false".
func AuthFarmEnrolled(auth *Auth) bool {
	if auth == nil || len(auth.Metadata) == 0 {
		return false
	}
	enrolled, ok := parseBoolAny(auth.Metadata[FarmEnrolledMetadataKey])
	if !ok {
		return false
	}
	return enrolled
}
