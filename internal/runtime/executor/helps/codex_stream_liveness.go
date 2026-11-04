package helps

import (
	"sync"
	"time"
)

// CodexStreamLiveness binds connection control traffic to the current main turn.
// No payload, account identity, URL or connection-lifetime history is retained.
type CodexStreamLiveness struct {
	mu     sync.Mutex
	active *CodexStreamStages
}

func (l *CodexStreamLiveness) Bind(stages *CodexStreamStages) func() {
	l.mu.Lock()
	l.active = stages
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		if l.active == stages {
			l.active = nil
		}
		l.mu.Unlock()
	}
}

func (l *CodexStreamLiveness) ObserveControl(kind string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active != nil {
		l.active.observeControl(kind)
	}
}

type CodexStreamLivenessSnapshot struct {
	PingSent, PingReceived, PongReceived, PingWriteErrors      uint64
	KeepAlives, TerminalErrorAttempts                          uint64
	DownstreamCanceled                                         bool
	UpstreamDataIdle, UpstreamControlIdle, DownstreamFlushIdle time.Duration
}

func (s *CodexStreamStages) observeControl(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.snapshot.Active || s.emitted {
		return
	}
	switch kind {
	case "ping_sent":
		s.snapshot.Liveness.PingSent++
	case "ping_received":
		s.snapshot.Liveness.PingReceived++
		s.lastControl = time.Now()
	case "pong_received":
		s.snapshot.Liveness.PongReceived++
		s.lastControl = time.Now()
	case "ping_write_error":
		s.snapshot.Liveness.PingWriteErrors++
	}
}

func (s *CodexStreamStages) ObserveKeepAlive() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.Active && !s.emitted {
		s.snapshot.Liveness.KeepAlives++
	}
}

// An attempted terminal write is not proof the client received the event.
func (s *CodexStreamStages) ObserveTerminalErrorAttempt() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.Active && !s.emitted {
		s.snapshot.Liveness.TerminalErrorAttempts++
	}
}

func (s *CodexStreamStages) ObserveDownstreamCanceled() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.Active && !s.emitted {
		s.snapshot.Liveness.DownstreamCanceled = true
	}
}

func (s *CodexStreamStages) livenessAt(now time.Time) CodexStreamLivenessSnapshot {
	v := s.snapshot.Liveness
	idle := func(last time.Time) time.Duration {
		if last.IsZero() {
			last = s.started
		}
		if last.IsZero() || now.Before(last) {
			return 0
		}
		return now.Sub(last)
	}
	v.UpstreamDataIdle = idle(s.lastData)
	v.UpstreamControlIdle = idle(s.lastControl)
	v.DownstreamFlushIdle = idle(s.lastFlush)
	return v
}
