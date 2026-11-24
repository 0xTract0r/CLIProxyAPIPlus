package management

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestVertexImportNumericRoundTripAndInvalidJSON(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	sa := map[string]any{"type": "service_account", "project_id": "ordinary-fixture", "client_email": "fixture@example.invalid", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), "unknown": map[string]any{"n": json.Number("9007199254740993"), "huge": json.Number("1e10000"), "flag": true, "list": []any{"a", json.Number("9007199254740995")}}}
	valid, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"v0", "v8"} {
		t.Run(version, func(t *testing.T) {
			for _, kind := range []string{"valid", "trailing", "invalid"} {
				t.Run(kind, func(t *testing.T) {
					dir := t.TempDir()
					store := sdkauth.NewFileTokenStore().WithBaseDir(dir)
					h := &Handler{cfg: &config.Config{AuthDir: dir}, tokenStore: store}
					router := gin.New()
					router.POST("/import", h.ImportVertexCredential)
					if version == "v8" {
						router = gin.New()
						router.POST("/import", h.ImportOAuthV8)
					}
					raw := valid
					if kind == "trailing" {
						raw = append(append([]byte(nil), valid...), []byte(" {}")...)
					}
					if kind == "invalid" {
						raw = []byte(`{"broken":`)
					}
					var body bytes.Buffer
					w := multipart.NewWriter(&body)
					part, _ := w.CreateFormFile("file", "synthetic.json")
					part.Write(raw)
					w.Close()
					req := httptest.NewRequest(http.MethodPost, "/import?provider=vertex", &body)
					req.Header.Set("Content-Type", w.FormDataContentType())
					rec := httptest.NewRecorder()
					router.ServeHTTP(rec, req)
					want := 200
					if kind != "valid" {
						want = 400
					}
					if rec.Code != want {
						t.Fatalf("status %d want %d", rec.Code, want)
					}
					files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
					if kind != "valid" {
						if len(files) != 0 {
							t.Fatal("invalid input persisted")
						}
						return
					}
					if len(files) != 1 {
						t.Fatal("missing file")
					}
					b, err := os.ReadFile(files[0])
					if err != nil {
						t.Fatal(err)
					}
					var doc struct {
						ServiceAccount struct {
							Unknown json.RawMessage `json:"unknown"`
						} `json:"service_account"`
					}
					if err = json.Unmarshal(b, &doc); err != nil {
						t.Fatal(err)
					}
					expected, _ := json.Marshal(sa["unknown"])
					var actual, wanted any
					d := json.NewDecoder(bytes.NewReader(doc.ServiceAccount.Unknown))
					d.UseNumber()
					d.Decode(&actual)
					d = json.NewDecoder(bytes.NewReader(expected))
					d.UseNumber()
					d.Decode(&wanted)
					a, _ := json.Marshal(actual)
					z, _ := json.Marshal(wanted)
					if !bytes.Equal(a, z) {
						t.Fatal("unknown JSON values changed")
					}
				})
			}
		})
	}
}
