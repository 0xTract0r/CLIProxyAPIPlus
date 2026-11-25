package executor

// Shared test transport for HTTP executors; no network is contacted by this adapter.
type roundTripperFunc = ordinaryCodexTestRoundTripper
