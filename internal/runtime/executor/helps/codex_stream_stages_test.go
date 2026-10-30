package helps

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	log "github.com/sirupsen/logrus"
)

type stageLogHook struct {
	mu         sync.Mutex
	messages   []string
	requestIDs []any
}

func (h *stageLogHook) Levels() []log.Level { return log.AllLevels }
func (h *stageLogHook) Fire(e *log.Entry) error {
	if strings.HasPrefix(e.Message, "codex stream stages ") {
		h.mu.Lock()
		h.messages = append(h.messages, e.Message)
		h.requestIDs = append(h.requestIDs, e.Data["request_id"])
		h.mu.Unlock()
	}
	return nil
}
func (h *stageLogHook) count() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.messages) }
func captureStageLogs(t *testing.T) *stageLogHook {
	t.Helper()
	l := log.StandardLogger()
	oldHooks := l.ReplaceHooks(make(log.LevelHooks))
	oldOut, oldLevel := l.Out, l.GetLevel()
	h := &stageLogHook{}
	l.AddHook(h)
	l.SetOutput(io.Discard)
	l.SetLevel(log.InfoLevel)
	t.Cleanup(func() { l.ReplaceHooks(oldHooks); l.SetOutput(oldOut); l.SetLevel(oldLevel) })
	return h
}

func TestCodexStreamStagesDoneAndInactive(t *testing.T) {
	for _, handlerFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "executor-first", true: "handler-first"}[handlerFirst], func(t *testing.T) {
			h := captureStageLogs(t)
			ctx, s := BeginCodexStreamStages(logging.WithRequestID(context.Background(), "stage001"), nil)
			StartCodexMainStream(ctx, time.Now(), nil)
			if handlerFirst {
				s.HandlerDone()
			} else {
				s.ExecutorDone("completed")
			}
			if h.count() != 0 {
				t.Fatal("summary emitted before both ends")
			}
			if handlerFirst {
				s.ExecutorDone("context_done")
			} else {
				s.HandlerDone()
			}
			s.HandlerDone()
			s.ExecutorDone("duplicate")
			if h.count() != 1 || h.requestIDs[0] != "stage001" {
				t.Fatal("summary missing, duplicated, or request ID lost")
			}
		})
	}
	t.Run("http-or-pre-main-error", func(t *testing.T) {
		h := captureStageLogs(t)
		_, s := BeginCodexStreamStages(nil, nil)
		s.ExecutorDone("prewarm_error")
		s.HandlerDone()
		if h.count() != 0 {
			t.Fatal("inactive execution emitted")
		}
	})
	t.Run("http-recovery", func(t *testing.T) {
		h := captureStageLogs(t)
		ctx, s := BeginCodexStreamStages(nil, nil)
		StartCodexMainStream(ctx, time.Now(), nil)
		s.SuppressHTTPRecovery()
		s.ExecutorDone("read_error")
		s.HandlerDone()
		if h.count() != 0 || s.Active() {
			t.Fatal("HTTP recovery retained active WS diagnostics")
		}
	})
}

func TestCodexStreamStagesWireClampCountersAndContextReuse(t *testing.T) {
	h := captureStageLogs(t)
	c := &gin.Context{}
	base := context.WithValue(logging.WithRequestID(context.Background(), "original"), "gin", c)
	ctx, s := BeginCodexStreamStages(base, c)
	started := time.Now()
	StartCodexMainStream(ctx, started, &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}})
	arrival := started.Add(10 * time.Millisecond)
	s.ObserveRead(started, arrival, time.Hour, false)
	v := s.Snapshot()
	if v.WireRead.Count != 1 || v.WireRead.Total != 10*time.Millisecond || v.Frames != 1 {
		t.Fatalf("wire idle not clipped: %+v", v)
	}
	s.mu.Lock()
	s.snapshot.Timeline.add(300 * time.Nanosecond)
	s.snapshot.Timeline.add(400 * time.Nanosecond)
	s.mu.Unlock()
	if v := s.Snapshot().Timeline; v.Count != 2 || v.Total != 700*time.Nanosecond || v.Max != 400*time.Nanosecond {
		t.Fatal("duration precision changed")
	}
	s.ObserveSend(time.Now())
	s.ObserveWrite(time.Now())
	s.ObserveFlush(time.Now())
	s.SetTimelineStorage("memory")
	// A recycled Gin context must not be consulted by the delayed executor end.
	s.HandlerDone()
	c.Keys = map[string]any{"request_id": "recycled"}
	s.ExecutorDone("completed")
	if h.count() != 1 || h.requestIDs[0] != "original" || !strings.Contains(h.messages[0], "timeline_storage=memory") {
		t.Fatal("summary used recycled request metadata")
	}
	nextCtx, next := BeginCodexStreamStages(base, c)
	StartCodexMainStream(nextCtx, time.Now(), nil)
	next.ExecutorDone("read_error")
	next.HandlerDone()
	if next.Snapshot().Frames != 0 || h.count() != 2 {
		t.Fatal("next request reused counters")
	}
}

func TestCodexStreamStagesRetryUsesOneSummary(t *testing.T) {
	h := captureStageLogs(t)
	ctx, s := BeginCodexStreamStages(nil, nil)
	StartCodexMainStream(ctx, time.Now(), nil)
	s.ExecutorDone("read_error")
	StartCodexMainStream(ctx, time.Now(), nil)
	s.ExecutorDone("completed")
	s.HandlerDone()
	if h.count() != 1 || s.Snapshot().MainAttempts != 2 {
		t.Fatal("retry did not converge to one summary")
	}
}
