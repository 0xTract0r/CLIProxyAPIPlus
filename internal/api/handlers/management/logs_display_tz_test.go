package management

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

// 写侧（logging.LogFormatter）用 displayLoc 渲染日志行前缀，读侧 parseTimestamp
// 必须用同一个 displayLoc 解析，否则后台日志查询的时间会按本机时区错位。
// 这里直接构造写侧渲染出的行前缀，断言读侧解析回的 Unix 与原始 UTC 时刻一致。
func TestParseTimestampMatchesDisplayLocation(t *testing.T) {
	t.Cleanup(func() {
		logging.SetDisplayTimezoneOffsetHours(logging.DefaultDisplayTimezoneOffsetHours)
	})

	// 默认 UTC+8。
	logging.SetDisplayTimezoneOffsetHours(logging.DefaultDisplayTimezoneOffsetHours)

	utc := time.Date(2026, 6, 16, 0, 30, 0, 0, time.UTC)
	// 写侧 LogFormatter 的口径：entry.Time.In(displayLoc).Format("2006-01-02 15:04:05")。
	prefix := utc.In(logging.DisplayLocation()).Format("2006-01-02 15:04:05")
	line := "[" + prefix + "] [--------] [info ] hello"

	got := parseTimestamp(line)
	if got != utc.Unix() {
		t.Fatalf("parseTimestamp(+8) = %d, want %d (line=%q)", got, utc.Unix(), line)
	}

	// 改成 UTC-5，写读两侧同步切换，仍应解析回同一 UTC 时刻。
	logging.SetDisplayTimezoneOffsetHours(-5)
	prefix = utc.In(logging.DisplayLocation()).Format("2006-01-02 15:04:05")
	line = "[" + prefix + "] [--------] [info ] hello"
	got = parseTimestamp(line)
	if got != utc.Unix() {
		t.Fatalf("parseTimestamp(-5) = %d, want %d (line=%q)", got, utc.Unix(), line)
	}
}
