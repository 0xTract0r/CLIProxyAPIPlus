//go:build darwin || linux

package api

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
)

func TestCredentialFieldsPreserveUnrelatedJSONOnDiskAndColdRead(t *testing.T) {
	f := newOrdinaryCredentialFixture(t, "")
	const source = `{
  "type": "synthetic",
  "disabled":false,
  "note": "初始备注",
  "priority":1,
  "request-retry":2,
  "legacy" : {"large":9007199254740993, "fraction":1.0000000000000000001, "extra":null}
}
`
	if err := os.WriteFile(f.path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	f.manager = f.newManager()
	if err := f.manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.server = f.newServer(f.manager)
	name := ordinaryCredentialFile
	response := ordinaryCredentialRequest(f.server, http.MethodPatch, ordinaryCredentialV0+"/fields", []byte(`{"name":"`+name+`","priority":9}`), true)
	if response.Code != http.StatusOK {
		t.Fatalf("PATCH /fields: %d %s", response.Code, response.Body.String())
	}
	disk, err := os.ReadFile(f.path)
	expected := bytes.Replace([]byte(source), []byte(`"priority":1`), []byte(`"priority":9`), 1)
	if err != nil || !bytes.Equal(disk, expected) {
		t.Fatalf("priority edit changed unrelated bytes: %s, err=%v", disk, err)
	}

	response = ordinaryCredentialRequest(f.server, http.MethodPatch, ordinaryCredentialV0+"/fields", []byte(`{"name":"`+name+`","weight":"bad","priority":12}`), true)
	rejected, err := os.ReadFile(f.path)
	if response.Code != http.StatusBadRequest || err != nil || !bytes.Equal(disk, rejected) {
		t.Fatalf("invalid patch changed disk: %d %s, err=%v", response.Code, response.Body.String(), err)
	}
	response = ordinaryCredentialRequest(f.server, http.MethodPatch, ordinaryCredentialV0+"/fields", []byte(`{"name":"`+name+`","request_retry":null,"legacy.extra":"added"}`), true)
	disk, err = os.ReadFile(f.path)
	if response.Code != http.StatusOK || err != nil || bytes.Contains(disk, []byte("request-retry")) ||
		!bytes.Contains(disk, []byte("9007199254740993")) || !bytes.Contains(disk, []byte("1.0000000000000000001")) {
		t.Fatalf("nested edit or legacy deletion failed: %d, %s, err=%v", response.Code, disk, err)
	}
	restarted := f.newManager()
	if err := restarted.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a, ok := restarted.GetByID(name); !ok {
		t.Fatal("cold store lost credential")
	} else {
		legacy, _ := a.Metadata["legacy"].(map[string]any)
		if n, ok := legacy["large"].(interface{ String() string }); !ok || n.String() != "9007199254740993" ||
			legacy["extra"] != "added" || a.Metadata["priority"] != float64(9) {
			t.Fatalf("cold credential lost precision or priority: %#v", legacy)
		}
	}

	synthesized, err := synthesizer.NewFileSynthesizer().Synthesize(&synthesizer.SynthesisContext{AuthDir: f.cfg.AuthDir, Config: f.cfg})
	if err != nil || len(synthesized) != 1 {
		t.Fatalf("watcher synthesis: %d files, err=%v", len(synthesized), err)
	}
	legacy, _ := synthesized[0].Metadata["legacy"].(map[string]any)
	if n, ok := legacy["large"].(interface{ String() string }); !ok || n.String() != "9007199254740993" ||
		synthesized[0].Attributes["priority"] != "9" {
		t.Fatalf("watcher credential lost precision or priority: %#v", legacy)
	}
}
