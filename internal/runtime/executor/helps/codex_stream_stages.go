package helps

import (
	"context"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

type codexStreamStagesContextKey struct{}

const codexStreamStagesGinKey = "__codex_stream_stages__"

// StreamStageDuration stores nanoseconds; rounding occurs only in the final log.
type StreamStageDuration struct {
	Count      uint64
	Total, Max time.Duration
}

func (d *StreamStageDuration) add(value time.Duration) {
	if value < 0 {
		value = 0
	}
	d.Count++
	d.Total += value
	if value > d.Max {
		d.Max = value
	}
}

type CodexStreamStageSnapshot struct {
	Active                                                           bool
	MainAttempts, Frames                                             uint64
	WireRead, ConsumerWait, ConsumeLag, Timeline, Send, Write, Flush StreamStageDuration
	Elapsed                                                          time.Duration
	Outcome, TimelineStorage                                         string
}

// CodexStreamStages is request-local diagnostics, separate from serving usage.
// Wire reads and queue lag overlap downstream work and must never be summed.
type CodexStreamStages struct {
	mu                                 sync.Mutex
	logger                             *log.Entry
	handlerOwned, handlerDone, emitted bool
	running                            uint64
	started                            time.Time
	snapshot                           CodexStreamStageSnapshot
}

// BeginCodexStreamStages remains inactive for HTTP and non-Codex execution.
func BeginCodexStreamStages(ctx context.Context, c *gin.Context) (context.Context, *CodexStreamStages) {
	if ctx == nil {
		ctx = context.Background()
	}
	s := &CodexStreamStages{logger: LogWithRequestID(ctx), handlerOwned: true}
	if c != nil {
		c.Set(codexStreamStagesGinKey, s)
	}
	return context.WithValue(ctx, codexStreamStagesContextKey{}, s), s
}

func CodexStreamStagesFromGin(c *gin.Context) *CodexStreamStages {
	if c == nil {
		return nil
	}
	value, _ := c.Get(codexStreamStagesGinKey)
	s, _ := value.(*CodexStreamStages)
	return s
}

// StartCodexMainStream is called only after a successful main websocket send.
func StartCodexMainStream(ctx context.Context, started time.Time, cfg *config.Config) *CodexStreamStages {
	if ctx == nil {
		ctx = context.Background()
	}
	s, _ := ctx.Value(codexStreamStagesContextKey{}).(*CodexStreamStages)
	if s == nil {
		s = &CodexStreamStages{logger: LogWithRequestID(ctx)}
		if c := ginContextFrom(ctx); c != nil {
			c.Set(codexStreamStagesGinKey, s)
		}
	}
	s.mu.Lock()
	s.snapshot.Active = true
	s.snapshot.MainAttempts++
	s.snapshot.TimelineStorage = "disabled"
	if requestLogCaptureEnabled(cfg) {
		if c := ginContextFrom(ctx); c != nil {
			if _, ok := apiWebsocketTimelineSource(c); ok {
				s.snapshot.TimelineStorage = "file"
			} else {
				s.snapshot.TimelineStorage = "memory"
			}
		}
	}
	s.running++
	if s.started.IsZero() {
		s.started = started
	}
	s.mu.Unlock()
	return s
}

func (s *CodexStreamStages) Active() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot.Active
}

func (s *CodexStreamStages) HandlerManaged() bool { return s != nil && s.handlerOwned }

func (s *CodexStreamStages) ObserveRead(waitStarted, arrival time.Time, wireRead time.Duration, failed bool) {
	if s == nil {
		return
	}
	consumed := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.ConsumerWait.add(consumed.Sub(waitStarted))
	if !arrival.IsZero() {
		s.snapshot.ConsumeLag.add(consumed.Sub(arrival))
		if readStarted := arrival.Add(-wireRead); readStarted.Before(s.started) {
			wireRead = arrival.Sub(s.started)
		}
		s.snapshot.WireRead.add(wireRead)
	}
	if !failed {
		s.snapshot.Frames++
	}
}

func (s *CodexStreamStages) ObserveTimeline(started time.Time) {
	if s != nil {
		s.mu.Lock()
		s.snapshot.Timeline.add(time.Since(started))
		s.mu.Unlock()
	}
}
func (s *CodexStreamStages) ObserveSend(started time.Time) {
	if s != nil {
		s.mu.Lock()
		s.snapshot.Send.add(time.Since(started))
		s.mu.Unlock()
	}
}
func (s *CodexStreamStages) ObserveWrite(started time.Time) {
	if s != nil {
		s.mu.Lock()
		s.snapshot.Write.add(time.Since(started))
		s.mu.Unlock()
	}
}
func (s *CodexStreamStages) ObserveFlush(started time.Time) {
	if s != nil {
		s.mu.Lock()
		s.snapshot.Flush.add(time.Since(started))
		s.mu.Unlock()
	}
}

func (s *CodexStreamStages) SetTimelineStorage(storage string) {
	if s != nil {
		s.mu.Lock()
		s.snapshot.TimelineStorage = storage
		s.mu.Unlock()
	}
}

// Recovered HTTP execution is outside this websocket-main diagnostic scope.
func (s *CodexStreamStages) SuppressHTTPRecovery() {
	if s != nil {
		s.mu.Lock()
		s.snapshot.Active = false
		s.running = 0
		s.mu.Unlock()
	}
}
func (s *CodexStreamStages) ExecutorDone(outcome string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.running == 0 {
		s.mu.Unlock()
		return
	}
	s.running--
	s.snapshot.Outcome = outcome
	s.mu.Unlock()
	s.emitIfFinished()
}

func (s *CodexStreamStages) HandlerDone() {
	if s != nil {
		s.mu.Lock()
		s.handlerDone = true
		s.mu.Unlock()
		s.emitIfFinished()
	}
}
func (s *CodexStreamStages) Snapshot() CodexStreamStageSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := s.snapshot
	if !s.started.IsZero() {
		snapshot.Elapsed = time.Since(s.started)
	}
	return snapshot
}

func (s *CodexStreamStages) emitIfFinished() {
	s.mu.Lock()
	if !s.snapshot.Active || s.running != 0 || (s.handlerOwned && !s.handlerDone) || s.emitted {
		s.mu.Unlock()
		return
	}
	s.emitted = true
	s.snapshot.Elapsed = time.Since(s.started)
	v := s.snapshot
	s.mu.Unlock()
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	s.logger.Infof("codex stream stages outcome=%s timeline_storage=%s main_attempts=%d frames=%d elapsed_main_ms=%.3f wire_read_count=%d wire_read_sum_ms=%.3f wire_read_max_ms=%.3f consumer_wait_sum_ms=%.3f consume_lag_count=%d consume_lag_max_ms=%.3f timeline_count=%d timeline_sum_ms=%.3f timeline_max_ms=%.3f send_count=%d send_sum_ms=%.3f send_max_ms=%.3f write_count=%d write_sum_ms=%.3f write_max_ms=%.3f flush_count=%d flush_sum_ms=%.3f flush_max_ms=%.3f", v.Outcome, v.TimelineStorage, v.MainAttempts, v.Frames, ms(v.Elapsed), v.WireRead.Count, ms(v.WireRead.Total), ms(v.WireRead.Max), ms(v.ConsumerWait.Total), v.ConsumeLag.Count, ms(v.ConsumeLag.Max), v.Timeline.Count, ms(v.Timeline.Total), ms(v.Timeline.Max), v.Send.Count, ms(v.Send.Total), ms(v.Send.Max), v.Write.Count, ms(v.Write.Total), ms(v.Write.Max), v.Flush.Count, ms(v.Flush.Total), ms(v.Flush.Max))
}
