package usage

// Telemetry describes executor-local observations, never provider-internal timings.
// Phase offsets are relative to StartedAtMS; connect/TLS are phase durations.
// Content chunks are protocol events, not tokens. Nil means not observed.
type Telemetry struct {
	Version           int      `json:"version"`
	AttemptID         string   `json:"attempt_id"`
	RequestID         string   `json:"request_id,omitempty"`
	StartedAtMS       int64    `json:"started_at_ms"`
	EndedAtMS         int64    `json:"ended_at_ms"`
	Transport         string   `json:"transport"`
	ObservationKind   string   `json:"observation_kind"`
	ObservedStages    []string `json:"observed_stages"`
	ConnectMS         *int64   `json:"connect_ms,omitempty"`
	TLSMS             *int64   `json:"tls_ms,omitempty"`
	ResponseHeadersMS *int64   `json:"response_headers_ms,omitempty"`
	FirstBodyMS       *int64   `json:"first_body_ms,omitempty"`
	FirstContentMS    *int64   `json:"first_content_ms,omitempty"`
	LastContentMS     *int64   `json:"last_content_ms,omitempty"`
	ContentChunks     *int64   `json:"content_chunks,omitempty"`
	MaxContentGapMS   *int64   `json:"max_content_gap_ms,omitempty"`
	StallCount        *int64   `json:"stall_count,omitempty"`
	StallDurationMS   *int64   `json:"stall_duration_ms,omitempty"`
	StallThresholdMS  *int64   `json:"stall_threshold_ms,omitempty"`
	StreamCompleted   *bool    `json:"stream_completed,omitempty"`
	FinishReason      string   `json:"finish_reason,omitempty"`
	FailureKind       string   `json:"failure_kind,omitempty"`
}
