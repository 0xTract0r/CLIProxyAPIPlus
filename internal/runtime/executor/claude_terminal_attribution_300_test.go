package executor

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"testing"
	"time"
)

type blockAttribution300 struct{ entered, release chan struct{} }

func (p *blockAttribution300) HandleUsage(_ context.Context, r usage.Record) {
	if r.Provider == "marker300" {
		close(p.entered)
		<-p.release
	}
}
func TestTerminal300CaptureOwnAuthAfterQueuedForeignUsage(t *testing.T) {
	m := usage.NewManager(8)
	defer m.Stop()
	b := &blockAttribution300{make(chan struct{}), make(chan struct{})}
	m.RegisterNamed("block", b)
	m.Publish(context.Background(), usage.Record{Provider: "marker300"})
	<-b.entered
	foreign := usage.Record{Provider: "claude", AuthID: "foreign-auth-300", RequestID: "foreign-request-300", Failed: true, Detail: usage.Detail{InputTokens: 5, OutputTokens: 2}}
	m.Publish(context.Background(), foreign)
	own := &captureClaudeUsagePlugin{records: make(chan usage.Record, 4), authID: "claude-terminal-translated-300"}
	all := &captureClaudeUsagePlugin{records: make(chan usage.Record, 4)}
	m.RegisterNamed("own", own)
	m.RegisterNamed("all", all)
	close(b.release)
	m.Publish(context.Background(), usage.Record{Provider: "claude", AuthID: "claude-terminal-translated-300", RequestID: "own-request-300", Detail: usage.Detail{InputTokens: 100, OutputTokens: 15}})
	select {
	case r := <-own.records:
		if r.AuthID != "claude-terminal-translated-300" || r.RequestID != "own-request-300" || r.Failed || r.Detail.InputTokens != 100 || r.Detail.OutputTokens != 15 {
			t.Errorf("capture took foreign usage: auth=%s request=%s failed=%t input=%d output=%d", r.AuthID, r.RequestID, r.Failed, r.Detail.InputTokens, r.Detail.OutputTokens)
		}
	case <-time.After(time.Second):
		t.Fatal("own queue stalled")
	}
	select {
	case r := <-all.records:
		if r.AuthID != foreign.AuthID || !r.Failed || r.Detail.InputTokens != 5 || r.Detail.OutputTokens != 2 {
			t.Errorf("foreign record changed/dropped: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("foreign record was lost")
	}
}

func TestUsage302AIStudioAuthFilterPreservesForeignDispatch(t *testing.T) {
	m := usage.NewManager(8)
	defer m.Stop()
	b := &blockAttribution300{make(chan struct{}), make(chan struct{})}
	m.RegisterNamed("barrier302", b)
	m.Publish(context.Background(), usage.Record{Provider: "marker300"})
	<-b.entered
	foreign := usage.Record{Provider: "aistudio", Model: "gemini-3.1-pro-preview", AuthID: "foreign302", RequestID: "foreign302", Failed: true, TTFT: 0, Detail: usage.Detail{InputTokens: 100, OutputTokens: 15}}
	m.Publish(context.Background(), foreign)
	// Both captures subscribe after foreign publication, proving dispatch-time snapshot.
	all := &captureAIStudioUsagePlugin{records: make(chan usage.Record, 4)}
	selected := &captureAIStudioUsagePlugin{records: make(chan usage.Record, 4)}
	m.RegisterNamed("all302", all)
	m.RegisterNamed("selected302", selected)
	close(b.release)
	m.Publish(context.Background(), usage.Record{Provider: "aistudio", Model: foreign.Model, AuthID: "own302", RequestID: "own302", TTFT: 50 * time.Millisecond, Detail: foreign.Detail})
	got := waitForAIStudioUsageRecord(t, selected.records, foreign.Model, "own302")
	if got.AuthID != "own302" || got.RequestID != "own302" || got.Failed || got.TTFT != 50*time.Millisecond || got.Detail.InputTokens != 100 || got.Detail.OutputTokens != 15 {
		t.Fatalf("wrong owned record: %+v", got)
	}
	select {
	case got := <-all.records:
		if got.AuthID != foreign.AuthID || got.RequestID != foreign.RequestID || !got.Failed || got.TTFT != 0 || got.Detail.InputTokens != 100 || got.Detail.OutputTokens != 15 {
			t.Fatalf("foreign production event altered/dropped: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("foreign event missing")
	}
}
