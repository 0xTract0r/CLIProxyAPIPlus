package cliproxy

import (
	"context"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestService279RunSingleBudgetAndShutdownRetry(t *testing.T) {
	// 进程级单例必须在虚拟时钟 bubble 外初始化，避免把常驻日志/usage 当作 Service 后代。
	cfg := &config.Config{}
	logging.ConfigureErrorLogAlert(cfg.ErrorLogAlert)
	usage.StartDefault(context.Background())
	t.Cleanup(usage.StopDefault)
	path := filepath.Join(t.TempDir(), "auth-file")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		stops := 0
		svc := &Service{cfg: &config.Config{AuthDir: path}, authQueueStop: func() { stops++ }}
		if !svc.beginAntigravityCallback() {
			t.Fatal("callback rejected")
		}
		start := time.Now()
		err := svc.Run(context.Background())
		elapsed := time.Since(start)
		if stops != 0 {
			t.Errorf("cleanup ran before quiescence: %d", stops)
		}
		svc.antigravityProbeWg.Done()
		if elapsed != 90*time.Second {
			t.Errorf("single Run exit budget: elapsed=%v want=90s", elapsed)
		}
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "auth path exists but is not a directory") {
			t.Errorf("must retain AuthDir AND first Shutdown error: %v", err)
		}
		// 超时不能消费 shutdownOnce；受控后代退出后，独立调用者的新 ctx 可正常完成。
		retryCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := svc.Shutdown(retryCtx); err != nil {
			t.Errorf("explicit retry: %v", err)
		}
		if stops != 1 {
			t.Errorf("retry did not perform deferred cleanup: %d", stops)
		}
		if err := svc.Shutdown(retryCtx); err != nil || stops != 1 {
			t.Errorf("repeated shutdown: err=%v cleanup=%d", err, stops)
		}
		t.Logf("actual Run elapsed=%v; original AuthDir + deadline retained; explicit retry completed", elapsed)
	})
}
