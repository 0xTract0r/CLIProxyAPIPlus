package helps

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	log "github.com/sirupsen/logrus"
)

func TestAPIWebsocketTimelineBytesAndFileCount(t *testing.T) {
	for _, disk := range []bool{false, true} {
		t.Run(fmt.Sprintf("disk=%t", disk), func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			var source *logging.FileBodySource
			if disk {
				var err error
				source, err = logging.NewFileBodySourceInDir(t.TempDir(), "timeline")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = source.Cleanup() }()
				ctx.Set(logging.APIWebsocketTimelineSourceContextKey, source)
			}
			parts := []string{" request\n{} \n", "\n", " response\n{\"delta\":\"synthetic\"} ", " error\nsynthetic read failure\n"}
			var expected []string
			for _, part := range parts {
				appendAPIWebsocketTimeline(ctx, []byte(part))
				if trimmed := strings.TrimSpace(part); trimmed != "" {
					expected = append(expected, trimmed)
				}
			}
			want := strings.Join(expected, "\n\n")
			var got []byte
			if disk {
				var err error
				got, err = source.Bytes()
				if err != nil {
					t.Fatal(err)
				}
				want += "\n"
			} else {
				value, _ := ctx.Get(apiWebsocketTimelineKey)
				got = value.([]byte)
			}
			if string(got) != want {
				t.Fatal("timeline bytes/separators changed")
			}
			if disk && len(source.Paths()) != 1 {
				t.Fatalf("timeline created %d files for 3 events; want one", len(source.Paths()))
			}
		})
	}
}

func TestAPIWebsocketTimelineMemoryPrefixSnapshot(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	appendAPIWebsocketTimeline(ctx, []byte("first"))
	value, _ := ctx.Get(apiWebsocketTimelineKey)
	prefix := value.([]byte)
	for i := 0; i < 128; i++ {
		appendAPIWebsocketTimeline(ctx, []byte(fmt.Sprintf("frame-%d", i)))
	}
	if !bytes.Equal(prefix, []byte("first")) {
		t.Fatal("appending mutated an existing prefix snapshot")
	}
}

func TestAPIWebsocketTimelineConcurrentMemoryAndDiskFailure(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	source, err := logging.NewFileBodySourceInDir(t.TempDir(), "failure")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Cleanup() }()
	ctx.Set(logging.APIWebsocketTimelineSourceContextKey, source)
	appendAPIWebsocketTimeline(ctx, []byte("prefix"))
	// Removing its path as a file forces a real mkdir failure on the next append.
	paths := source.Paths()
	if err := os.RemoveAll(filepath.Dir(paths[0])); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(paths[0]), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	appendAPIWebsocketTimeline(ctx, []byte("fallback"))
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); appendAPIWebsocketTimeline(ctx, []byte(fmt.Sprintf("memory-%d", i))) }(i)
	}
	wg.Wait()
	value, _ := ctx.Get(apiWebsocketTimelineKey)
	data := value.([]byte)
	if !bytes.HasPrefix(data, []byte("fallback\n\n")) || bytes.Count(data, []byte("memory-")) != 64 {
		t.Fatal("fallback/concurrent memory append lost events")
	}
}

func TestAPIWebsocketTimelineDoubleIOFailureStaysMemory(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	source, err := logging.NewFileBodySourceInDir(t.TempDir(), "double-failure")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Cleanup() }()
	ctx.Set(logging.APIWebsocketTimelineSourceContextKey, source)
	appendAPIWebsocketTimeline(ctx, []byte("unavailable A"))
	dir := filepath.Dir(source.Paths()[0])
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	logger := log.StandardLogger()
	previous := logger.Out
	var warnings bytes.Buffer
	logger.SetOutput(&warnings)
	t.Cleanup(func() { logger.SetOutput(previous) })
	appendAPIWebsocketTimeline(ctx, []byte("B"))
	if value, _ := ctx.Get(logging.APIWebsocketTimelineSourceContextKey); value != nil {
		t.Fatal("failed disk source remained active")
	}
	if !strings.Contains(warnings.String(), "coverage=partial_memory") {
		t.Fatal("unavailable prefix was not explicitly reported")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	} // The filesystem recovers.
	appendAPIWebsocketTimeline(ctx, []byte("C"))
	value, _ := ctx.Get(apiWebsocketTimelineKey)
	if string(value.([]byte)) != "B\n\nC" || source.HasPayload() {
		t.Fatal("disk recovery reordered memory events or resurrected the failed source")
	}
}

func BenchmarkAPIWebsocketTimelineLongPrefix(b *testing.B) {
	for _, disk := range []bool{false, true} {
		for _, frames := range []int{200, 1000, 2000} {
			b.Run(fmt.Sprintf("disk=%t/frames=%d", disk, frames), func(b *testing.B) {
				baseDir := b.TempDir()
				prefix := bytes.Repeat([]byte("p"), 3<<20)
				frame := bytes.Repeat([]byte("f"), 256)
				b.ReportAllocs()
				b.SetBytes(int64(len(prefix) + frames*len(frame)))
				for i := 0; i < b.N; i++ {
					ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
					var source *logging.FileBodySource
					if disk {
						var err error
						source, err = logging.NewFileBodySourceInDir(baseDir, "bench")
						if err != nil {
							b.Fatal(err)
						}
						ctx.Set(logging.APIWebsocketTimelineSourceContextKey, source)
					}
					appendAPIWebsocketTimeline(ctx, prefix)
					for n := 0; n < frames; n++ {
						appendAPIWebsocketTimeline(ctx, frame)
					}
					if disk {
						b.ReportMetric(float64(len(source.Paths())), "files/request")
						if err := source.Cleanup(); err != nil {
							b.Fatal(err)
						}
					} else {
						b.ReportMetric(0, "files/request")
					}
				}
			})
		}
	}
}
