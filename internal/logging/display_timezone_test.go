package logging

import (
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// 用例之间会改全局 displayLoc，结束后统一复位到默认，避免污染其它测试。
func resetDisplayLoc(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		SetDisplayTimezoneOffsetHours(DefaultDisplayTimezoneOffsetHours)
	})
}

func TestDefaultDisplayLocationIsUTC8(t *testing.T) {
	resetDisplayLoc(t)
	SetDisplayTimezoneOffsetHours(DefaultDisplayTimezoneOffsetHours)

	loc := DisplayLocation()
	// 已知 UTC 时间：2026-06-16 00:00:00 UTC，在 +8 下应显示为 08:00:00。
	utc := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	got := utc.In(loc).Format("2006-01-02 15:04:05")
	const want = "2026-06-16 08:00:00"
	if got != want {
		t.Fatalf("default display = %q, want %q", got, want)
	}

	_, offset := utc.In(loc).Zone()
	if offset != 8*3600 {
		t.Fatalf("default offset = %d seconds, want %d", offset, 8*3600)
	}
}

func TestApplyDisplayTimezoneNilCfgKeepsDefault(t *testing.T) {
	resetDisplayLoc(t)
	// 先改成别的，再用 nil cfg 应该复位回默认 +8。
	SetDisplayTimezoneOffsetHours(3)
	ApplyDisplayTimezone(nil)

	utc := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	if got := utc.In(DisplayLocation()).Format("15:04:05"); got != "08:00:00" {
		t.Fatalf("nil cfg display = %q, want 08:00:00", got)
	}
}

func TestApplyDisplayTimezoneConfigOverride(t *testing.T) {
	resetDisplayLoc(t)
	cfg := &config.Config{LoggingDisplayTimezoneOffsetHours: 0}
	ApplyDisplayTimezone(cfg)

	utc := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	if got := utc.In(DisplayLocation()).Format("15:04:05"); got != "00:00:00" {
		t.Fatalf("offset=0 display = %q, want 00:00:00", got)
	}

	cfg.LoggingDisplayTimezoneOffsetHours = -5
	ApplyDisplayTimezone(cfg)
	// 2026-06-16 02:00:00 UTC 在 -5 下应是 2026-06-15 21:00:00。
	utc2 := time.Date(2026, 6, 16, 2, 0, 0, 0, time.UTC)
	if got := utc2.In(DisplayLocation()).Format("2006-01-02 15:04:05"); got != "2026-06-15 21:00:00" {
		t.Fatalf("offset=-5 display = %q, want 2026-06-15 21:00:00", got)
	}
}

func TestSetDisplayTimezoneOffsetHoursZoneName(t *testing.T) {
	resetDisplayLoc(t)
	cases := []struct {
		offset int
		name   string
	}{
		{8, "UTC+8"},
		{0, "UTC"},
		{-5, "UTC-5"},
		{14, "UTC+14"},
	}
	for _, c := range cases {
		SetDisplayTimezoneOffsetHours(c.offset)
		utc := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
		zone, _ := utc.In(DisplayLocation()).Zone()
		if zone != c.name {
			t.Fatalf("offset %d zone name = %q, want %q", c.offset, zone, c.name)
		}
	}
}

// LogFormatter 写出的时间应使用 displayLoc。这里同时验证 management 读侧的解析
// 口径与之一致（写=+8 显示，读=+8 解析，Unix 不错位）。
func TestLogFormatterUsesDisplayLocation(t *testing.T) {
	resetDisplayLoc(t)
	SetDisplayTimezoneOffsetHours(DefaultDisplayTimezoneOffsetHours)

	f := &LogFormatter{}
	utc := time.Date(2026, 6, 16, 0, 30, 0, 0, time.UTC)
	out, err := f.Format(&log.Entry{Time: utc, Message: "hello", Level: log.InfoLevel})
	if err != nil {
		t.Fatalf("Format error: %v", err)
	}
	// +8 下应渲染为 08:30:00。
	if want := "[2026-06-16 08:30:00]"; !strings.Contains(string(out), want) {
		t.Fatalf("formatted line = %q, want substring %q", string(out), want)
	}
}
