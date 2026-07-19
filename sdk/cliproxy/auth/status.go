package auth

// Status represents the lifecycle state of an Auth entry.
type Status string

const (
	// StatusUnknown means the auth state could not be determined.
	StatusUnknown Status = "unknown"
	// StatusActive indicates the auth is valid and ready for execution.
	StatusActive Status = "active"
	// StatusPending indicates the auth is waiting for an external action, such as MFA.
	StatusPending Status = "pending"
	// StatusRefreshing indicates the auth is undergoing a refresh flow.
	StatusRefreshing Status = "refreshing"
	// StatusError indicates the auth is temporarily unavailable due to errors.
	StatusError Status = "error"
	// StatusDisabled marks the auth as intentionally disabled.
	StatusDisabled Status = "disabled"
	// StatusQuarantined marks the auth as automatically quarantined by the
	// auth manager after repeated terminal authentication failures (e.g. a
	// revoked OAuth token). It is distinct from StatusDisabled: the operator
	// never chose this, and it is automatically lifted the moment the
	// credential is re-authenticated or produces a real successful request.
	// See Auth.AutoQuarantined.
	StatusQuarantined Status = "quarantined"
)
