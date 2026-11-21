package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"gopkg.in/yaml.v3"
)

func TestHandlerSnapshotOwnsConfigAndPluginYAML(t *testing.T) {
	const root = "port: 16321\nplugins:\n  configs:\n    synthetic:\n      enabled: true\n      custom-note: original\n"
	var caller config.Config
	if err := yaml.Unmarshal([]byte(root), &caller); err != nil {
		t.Fatal(err)
	}
	h := &Handler{}
	h.SetConfig(&caller)
	snapshot := h.configSnapshot()
	entry := snapshot.Plugins.Configs["synthetic"]
	if entry.Enabled == nil || !*entry.Enabled || entry.Raw.Kind != yaml.MappingNode || len(entry.Raw.Content) < 2 {
		t.Fatal("snapshot lost ordinary plugin metadata")
	}
	*snapshot.Plugins.Configs["synthetic"].Enabled = false
	entry.Raw.Content[1].Value = "changed-in-copy"
	snapshot.Port = 1
	delete(snapshot.Plugins.Configs, "synthetic")

	next := h.configSnapshot()
	fixture := next.Plugins.Configs["synthetic"]
	if next.Port != 16321 || fixture.Enabled == nil || !*fixture.Enabled || fixture.Raw.Content[1].Value == "changed-in-copy" {
		t.Fatal("config snapshot caller changed the handler's port, map, pointer or YAML")
	}
	if caller.Port != 16321 || !*caller.Plugins.Configs["synthetic"].Enabled {
		t.Fatal("SetConfig caller lost its original config")
	}
}

func TestHandlerSnapshotGetConfigDuringSetConfig(t *testing.T) {
	h := &Handler{}
	engine := gin.New()
	engine.GET("/config", h.GetConfig)
	read := func() bool {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
		var out map[string]any
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			return false
		}
		if len(out) == 0 {
			return true
		}
		return out["debug"] == true || out["debug"] == false
	}
	if !read() {
		t.Fatal("unexpected empty config response")
	}
	start := make(chan struct{})
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		<-start
		for i := 0; i < 200; i++ {
			h.SetConfig(&config.Config{Port: 16321, Debug: i%2 == 0})
			if i%7 == 0 {
				h.SetConfig(nil)
			}
		}
	}()
	close(start)
	for i := 0; i < 200; i++ {
		if !read() {
			t.Error("invalid normal configuration JSON")
		}
	}
	done.Wait()
}

func TestHandlerSnapshotCatalogDuringPluginHostSwap(t *testing.T) {
	const channel = "fixture-snapshot-catalog"
	global := registry.GetGlobalRegistry()
	global.RegisterClient(channel, channel, []*registry.ModelInfo{
		{ID: "fixture-z", Object: "model", Type: "chat"},
		{ID: "fixture-a", Object: "model", Type: "chat"},
	})
	t.Cleanup(func() { global.UnregisterClient(channel) })
	h := &Handler{}
	engine := gin.New()
	engine.GET("/model-definitions/:channel", h.GetStaticModelDefinitions)
	var done sync.WaitGroup
	start := make(chan struct{})
	done.Add(1)
	go func() {
		defer done.Done()
		<-start
		empty := &pluginhost.Host{}
		for i := 0; i < 100; i++ {
			h.SetPluginHost(empty)
			h.SetPluginHost(nil)
		}
	}()
	close(start)
	for i := 0; i < 100; i++ {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/model-definitions/"+channel, nil))
		var out struct {
			Channel string
			Models  []struct{ ID string }
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil ||
			out.Channel != channel || len(out.Models) != 2 ||
			out.Models[0].ID != "fixture-a" || out.Models[1].ID != "fixture-z" {
			t.Errorf("ordinary registry fallback returned an unexpected result: %s", rec.Body.String())
		}
	}
	done.Wait()
}
