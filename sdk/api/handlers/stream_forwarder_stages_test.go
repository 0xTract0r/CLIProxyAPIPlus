package handlers

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
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
			}
		})
	}
}
