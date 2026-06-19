package logging

import (
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// 显示时区边界说明（必读）：
//
// displayLoc 只用于「人看的后端日志」时间显示（logrus 标准日志、gin access log、
// request log 内容时间戳、转发/告警时间、后台日志查询解析）。它默认 UTC+8，
// 方便本地运维直接读懂日志时间。
//
// 它**绝不能**用于任何会出站到上游（Anthropic / Kiro 等）的时间字段：Kiro 注入
// 进出站 system prompt 的 `[Context: Current time is ...]` 是上游观察到的时间画像，
// 属于反关联指纹边界，必须保持其原有时区口径，禁止经过 displayLoc。
const (
	// DefaultDisplayTimezoneOffsetHours 是「人看的日志」默认显示时区偏移（小时）。
	// 默认 8 表示 UTC+8（东八区），可由 config 的
	// logging-display-timezone-offset-hours 覆盖。
	DefaultDisplayTimezoneOffsetHours = 8

	// displayTimezoneName 是构造 FixedZone 时使用的时区名（仅用于显示标签）。
	displayTimezoneName = "UTC+8"
)

var (
	displayLocMu sync.RWMutex
	// displayLoc 是「人看的日志」统一显示时区，默认 UTC+8。
	displayLoc = time.FixedZone(displayTimezoneName, DefaultDisplayTimezoneOffsetHours*3600)
)

// DisplayLocation 返回当前「人看的日志」显示时区。
// 写侧格式化与读侧解析都应使用它，保证写/读口径一致。
func DisplayLocation() *time.Location {
	displayLocMu.RLock()
	defer displayLocMu.RUnlock()
	return displayLoc
}

// SetDisplayTimezoneOffsetHours 设置「人看的日志」显示时区偏移（小时）。
// 仅影响日志显示与后台日志解析，不影响任何出站时间字段。
func SetDisplayTimezoneOffsetHours(offsetHours int) {
	name := fixedZoneName(offsetHours)
	loc := time.FixedZone(name, offsetHours*3600)
	displayLocMu.Lock()
	displayLoc = loc
	displayLocMu.Unlock()
}

// ApplyDisplayTimezone 从 config 应用显示时区覆盖。cfg 为 nil 或未显式配置时，
// 维持默认 UTC+8。它在 ConfigureLogOutput 内被调用，因此启动和配置热重载都会生效。
func ApplyDisplayTimezone(cfg *config.Config) {
	if cfg == nil {
		SetDisplayTimezoneOffsetHours(DefaultDisplayTimezoneOffsetHours)
		return
	}
	SetDisplayTimezoneOffsetHours(cfg.LoggingDisplayTimezoneOffsetHours)
}

// fixedZoneName 为给定偏移生成稳定可读的时区标签，例如 +8 -> "UTC+8"，
// -5 -> "UTC-5"，0 -> "UTC"。
func fixedZoneName(offsetHours int) string {
	if offsetHours == 0 {
		return "UTC"
	}
	sign := "+"
	v := offsetHours
	if v < 0 {
		sign = "-"
		v = -v
	}
	return "UTC" + sign + itoa(v)
}

// itoa 是不引入 strconv 的小整数转字符串（offsetHours 范围有限）。
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
