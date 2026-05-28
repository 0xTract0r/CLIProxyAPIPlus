package logging

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

func TestFeishuErrorHookSendsOnlyErrorAndRedactsSecrets(t *testing.T) {
	requests := make(chan []byte, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
			t.Fatalf("content-type = %q, want application/json", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		requests <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	hook := newFeishuErrorHook(server.Client())
	defer hook.close()
	hook.Configure(config.ErrorLogAlertConfig{FeishuWebhookURL: server.URL})

	logger := log.New()
	logger.SetOutput(io.Discard)
	logger.SetLevel(log.DebugLevel)
	logger.SetReportCaller(true)
	logger.AddHook(hook)

	logger.Warn("refresh warning should not alert")
	select {
	case body := <-requests:
		t.Fatalf("warn log unexpectedly sent alert: %s", string(body))
	case <-time.After(100 * time.Millisecond):
	}

	logger.WithFields(log.Fields{
		"auth_id":       "codex-user@example.com-plus.json",
		"refresh_token": "secret-refresh-token",
		"body_preview":  `{"access_token":"secret-access-token","message":"bad"}`,
	}).Error("token refresh failed: Authorization: Bearer secret-bearer-token")

	var body []byte
	select {
	case body = <-requests:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for error alert")
	}

	var payload feishuTextPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v; body=%s", err, string(body))
	}
	if payload.MsgType != "text" {
		t.Fatalf("msg_type = %q, want text", payload.MsgType)
	}
	text := payload.Content.Text
	for _, secret := range []string{"secret-refresh-token", "secret-access-token", "secret-bearer-token"} {
		if strings.Contains(text, secret) {
			t.Fatalf("alert text leaked secret %q: %s", secret, text)
		}
	}
	for _, want := range []string{"level=error", "token refresh failed", "auth_id=codex-user@example.com-plus.json", "refresh_token=<redacted>", "access_token=<redacted>"} {
		if !strings.Contains(text, want) {
			t.Fatalf("alert text missing %q: %s", want, text)
		}
	}
}

func TestFeishuErrorHookDisabledWhenWebhookEmpty(t *testing.T) {
	hook := newFeishuErrorHook(nil)
	defer hook.close()
	hook.Configure(config.ErrorLogAlertConfig{})

	logger := log.New()
	logger.SetOutput(io.Discard)
	logger.SetLevel(log.DebugLevel)
	logger.AddHook(hook)

	logger.Error("error without webhook should be ignored")
}
