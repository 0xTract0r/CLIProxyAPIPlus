package api

import (
	"testing"
	"time"
)

// TestNewInboundHTTP2ServerKeepaliveEnabled 是 fork 存活守卫测试。
//
// 背景:入站 HTTP/2 配置若被上游同步还原成空的 &http2.Server{},ReadIdleTimeout
// 与 PingTimeout 都会回到 0——服务端既不发 keepalive PING 焐热空闲连接,也不主动
// 探死清理。被中间网络静默掐断的空闲入站连接会在客户端(如维持长连接池的 codex CLI)
// 复用时报 "error sending request"。
//
// 本测试直接断言 newInboundHTTP2Server() 的两个保活字段仍为正值,作为
// scripts/upstream-sync/fork-feature-manifest.tsv 存活审计之外的行为级兜底:
// 一旦有人把配置改回空值(或把字段清零),此测试立即变红,防止该 fork 稳定性修复
// 在上游同步/重构中被静默回退。
func TestNewInboundHTTP2ServerKeepaliveEnabled(t *testing.T) {
	h2 := newInboundHTTP2Server()
	if h2 == nil {
		t.Fatal("newInboundHTTP2Server() 返回 nil;入站 HTTP/2 保活配置丢失")
	}

	// ReadIdleTimeout 必须为正:否则服务端永远不会对空闲连接发 keepalive PING。
	if h2.ReadIdleTimeout <= 0 {
		t.Errorf("ReadIdleTimeout=%v,必须为正值以启用空闲连接 keepalive PING;"+
			"疑似被还原为空 http2.Server 配置", h2.ReadIdleTimeout)
	}

	// PingTimeout 必须为正:否则即使发了 PING,也没有判死超时来关闭死连接。
	if h2.PingTimeout <= 0 {
		t.Errorf("PingTimeout=%v,必须为正值以在 PING 未收到 ACK 时判死并关闭连接;"+
			"疑似被还原为空 http2.Server 配置", h2.PingTimeout)
	}

	// 上界护栏:空闲探活间隔过长会让"焐热/探死"失去意义(接近永不探活),
	// 因此约束在 60s 以内,避免未来无意中把它调成远大于连接空闲窗口的值。
	if h2.ReadIdleTimeout > 60*time.Second {
		t.Errorf("ReadIdleTimeout=%v 超过 60s 上界护栏;空闲探活间隔过长会削弱保活效果",
			h2.ReadIdleTimeout)
	}
}
