package handlers

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

type stageBarrierFlusher struct {
	entered, release chan struct{}
	once             sync.Once
	count            int
}

func (f *stageBarrierFlusher) Flush() { f.count++; f.once.Do(func() { close(f.entered); <-f.release }) }

func TestForwardStreamStagesWriteFlushAndCancel(t *testing.T) {
	for _, mode := range []string{"write", "flush", "terminal-error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
			stageCtx, stages := helps.BeginCodexStreamStages(ctx, c)
			helps.StartCodexMainStream(stageCtx, time.Now(), nil)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			flusher := &stageBarrierFlusher{entered: entered, release: release}
			if mode != "flush" {
				flusher.once.Do(func() {})
			}
			data := make(chan []byte, 1)
			errs := make(chan *interfaces.ErrorMessage, 1)
			if mode == "terminal-error" {
				errs <- &interfaces.ErrorMessage{Error: errors.New("synthetic"), StatusCode: 500}
			} else if mode != "cancel" {
				data <- []byte("unchanged")
			}
			if mode != "cancel" {
				close(data)
			}
			zero := time.Duration(0)
			write := func(chunk []byte) {
				if mode == "write" {
					close(entered)
					<-release
				}
				_, _ = recorder.Write(chunk)
			}
			done := make(chan struct{})
			go func() {
				(&BaseAPIHandler{}).ForwardStream(c, flusher, func(error) {}, data, errs, StreamForwardOptions{KeepAliveInterval: &zero, WriteChunk: write, WriteTerminalError: func(*interfaces.ErrorMessage) { _, _ = recorder.Write([]byte("terminal")) }, WriteDone: func() { _, _ = recorder.Write([]byte("done")) }})
				close(done)
			}()
			if mode == "write" || mode == "flush" {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("barrier not reached")
				}
				select {
				case <-done:
					t.Fatal("slow downstream callback did not block forwarding")
				default:
				}
				unblock()
			} else if mode == "cancel" {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("forwarding did not finish")
			}
			stages.ExecutorDone("completed")
			stages.HandlerDone()
			v := stages.Snapshot()
			if mode == "write" || mode == "flush" {
				if recorder.Body.String() != "unchangeddone" || v.Write.Count != 2 || v.Flush.Count != 2 || v.Write.Total <= 0 || v.Flush.Total <= 0 {
					t.Fatalf("callback bytes/counters changed: %q %+v", recorder.Body.String(), v)
				}
			} else if mode == "terminal-error" {
				if recorder.Body.String() != "terminal" || v.Write.Count != 1 || v.Flush.Count != 1 {
					t.Fatal("terminal write/flush changed")
				}
			} else if !v.Liveness.DownstreamCanceled || v.Write.Count != 0 || v.Flush.Count != 0 {
				t.Fatalf("downstream cancellation stages: %+v", v)
			}
		})
	}
}

func TestForwardStreamClosedErrorsSleepsUntilData(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		data := make(chan []byte)
		errs := make(chan *interfaces.ErrorMessage)
		close(errs)
		done := make(chan struct{})
		go func() {
			(&BaseAPIHandler{}).ForwardStream(c, c.Writer, func(error) {}, data, errs, StreamForwardOptions{})
			close(done)
		}()
		// A closed error channel must leave the forwarder blocked, not spinning.
		synctest.Wait()
		close(data)
		<-done
	})
}

func TestForwardStreamFinalizesOnceAndCountsFailedFlush(t *testing.T) {
	terminalErr := errors.New("upstream failed")
	flushErr := errors.New("downstream closed")
	for _, test := range []struct {
		name                 string
		terminalChunk        bool
		closeErr             error
		flushErr             error
		wantCancel           error
		wantBody             string
		wantTerminalAttempts uint64
	}{
		{name: "clean close", wantBody: "done"},
		{name: "terminal chunk", terminalChunk: true, wantBody: "chunk"},
		{name: "close validation failure", closeErr: terminalErr, wantCancel: terminalErr, wantBody: "error", wantTerminalAttempts: 1},
		{name: "failed terminal flush", closeErr: terminalErr, flushErr: flushErr, wantCancel: flushErr, wantBody: "error", wantTerminalAttempts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("POST", "/stream", nil)
			ctx, stages := helps.BeginCodexStreamStages(c.Request.Context(), c)
			helps.StartCodexMainStream(ctx, time.Now(), nil)
			data := make(chan []byte, 1)
			if test.terminalChunk {
				data <- []byte("chunk")
			}
			close(data)
			zero := time.Duration(0)
			var seen []error
			var flushes int
			(&BaseAPIHandler{}).ForwardStream(c, recorder, func(err error) { seen = append(seen, err) }, data, nil, StreamForwardOptions{
				KeepAliveInterval: &zero,
				WriteChunk:        func(chunk []byte) { _, _ = c.Writer.Write(chunk) },
				ChunkDone:         func() bool { return test.terminalChunk },
				CloseError: func() *interfaces.ErrorMessage {
					if test.closeErr == nil {
						return nil
					}
					return &interfaces.ErrorMessage{Error: test.closeErr}
				},
				WriteTerminalError: func(*interfaces.ErrorMessage) { _, _ = c.Writer.WriteString("error") },
				WriteDone:          func() { _, _ = c.Writer.WriteString("done") },
				Flush:              func() error { flushes++; return test.flushErr },
			})
			snapshot := stages.Snapshot()
			if len(seen) != 1 || !errors.Is(seen[0], test.wantCancel) {
				t.Fatalf("unexpected final outcome: %v", seen)
			}
			if recorder.Body.String() != test.wantBody || flushes != 1 || snapshot.Write.Count != 1 || snapshot.Flush.Count != 1 {
				t.Fatalf("body %q, flushes %d, stages %+v", recorder.Body.String(), flushes, snapshot)
			}
			if snapshot.Liveness.TerminalErrorAttempts != test.wantTerminalAttempts || snapshot.Liveness.DownstreamCanceled != (test.flushErr != nil) {
				t.Fatalf("liveness %+v", snapshot.Liveness)
			}
		})
	}
}
