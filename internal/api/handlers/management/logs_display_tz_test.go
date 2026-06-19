package management

import (
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"

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
	// 写侧 LogFormatter 的口径：带 +08:00 时区后缀，形如
	// `[2026-06-16 08:30:00 +08:00] ...`。读侧只取前 19 字符，后缀不应干扰解析。
	prefix := utc.In(logging.DisplayLocation()).Format("2006-01-02 15:04:05 -07:00")
	line := "[" + prefix + "] [--------] [info ] hello"
	if want := "[2026-06-16 08:30:00 +08:00]"; line[:len(want)] != want {
		t.Fatalf("constructed write-side line = %q, want prefix %q", line, want)
	}

	got := parseTimestamp(line)
	if got != utc.Unix() {
		t.Fatalf("parseTimestamp(+8) = %d, want %d (line=%q)", got, utc.Unix(), line)
	}

	// 改成 UTC-5，写读两侧同步切换，行前缀后缀也变为 -05:00，仍应解析回同一 UTC 时刻。
	logging.SetDisplayTimezoneOffsetHours(-5)
	prefix = utc.In(logging.DisplayLocation()).Format("2006-01-02 15:04:05 -07:00")
	line = "[" + prefix + "] [--------] [info ] hello"
	got = parseTimestamp(line)
	if got != utc.Unix() {
		t.Fatalf("parseTimestamp(-5) = %d, want %d (line=%q)", got, utc.Unix(), line)
	}
}

// 直接用真实 LogFormatter 渲染的日志行喂给读侧 parseTimestamp，端到端验证
// 写侧加了时区后缀后读侧仍能解析回正确 Unix 时刻（写读一致的最强保证）。
func TestParseTimestampOnRealFormatterLine(t *testing.T) {
	t.Cleanup(func() {
		logging.SetDisplayTimezoneOffsetHours(logging.DefaultDisplayTimezoneOffsetHours)
	})
	logging.SetDisplayTimezoneOffsetHours(logging.DefaultDisplayTimezoneOffsetHours)

	utc := time.Date(2026, 6, 16, 0, 30, 0, 0, time.UTC)
	f := &logging.LogFormatter{}
	out, err := f.Format(&log.Entry{Time: utc, Message: "hello", Level: log.InfoLevel})
	if err != nil {
		t.Fatalf("Format error: %v", err)
	}
	line := strings.TrimRight(string(out), "\r\n")
	if got := parseTimestamp(line); got != utc.Unix() {
		t.Fatalf("parseTimestamp(real line) = %d, want %d (line=%q)", got, utc.Unix(), line)
	}
}
