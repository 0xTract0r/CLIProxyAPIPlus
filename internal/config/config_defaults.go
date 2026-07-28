package config

import "time"

const (
	DefaultPanelGitHubRepository = "https://github.com/router-for-me/Cli-Proxy-API-Management-Center"
	DefaultPprofAddr             = "127.0.0.1:8316"
	DefaultAuthDir               = "~/.cli-proxy-api"
	DefaultErrorLogsMaxFiles     = 10
	DefaultLogsCompressAfterDays = 7
	DefaultLogsDeleteAfterDays   = 30
	// DefaultLoggingDisplayTimezoneOffsetHours 是「人看的日志」默认显示时区偏移（小时）。
	// 默认 8 = UTC+8（东八区）。只影响日志显示/解析，不影响出站时间字段。
	DefaultLoggingDisplayTimezoneOffsetHours = 8
)

const (
	DefaultQuotaSnapshotRefreshInterval            = 45 * time.Minute
	DefaultQuotaSnapshotRefreshJitter              = 10 * time.Minute
	DefaultQuotaSnapshotRefreshStartupMaxStaleness = 24 * time.Hour

	DefaultQuotaSnapshotRefreshIntervalString            = "45m"
	DefaultQuotaSnapshotRefreshJitterString              = "10m"
	DefaultQuotaSnapshotRefreshStartupMaxStalenessString = "24h"
)

const (
	ClaudeSonnetLongContextPolicyFailWithHint  = "fail_with_hint"
	ClaudeSonnetLongContextPolicyRouteToOpus1M = "route_to_opus_1m"
	ClaudeSonnetLongContextPolicyCompact       = "compact_required"
)
