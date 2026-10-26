package executor
import("bytes";"context";"crypto/sha256";"fmt";"io";"net/http";"net/http/httptest";"testing"
 "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
 "github.com/router-for-me/CLIProxyAPI/v8/internal/util"
 auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
 exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
 translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator")
func TestClaudeCount237NativeAutoPreservesPayloadAliases(t *testing.T){
 resetClaudeDeviceProfileCache();t.Cleanup(resetClaudeDeviceProfileCache)
 captured:=make(chan []byte,2)
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){if r.Method!="POST"||r.URL.Path!="/v1/messages/count_tokens"{t.Errorf("unexpected request %s %s",r.Method,r.URL.Path)};b,_:=io.ReadAll(r.Body);captured<-b;w.Header().Set("Content-Type","application/json");_,_=io.WriteString(w,`{"input_tokens":9}`)}));defer server.Close()
 raw:=[]byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"text","text":"alias237"}]}]}`)
 payload:=make([]byte,len(raw),len(raw)+128);copy(payload,raw)
 shared:=payload[:];snapshot:=bytes.Clone(payload);alias:=util.ParseGJSONBytesNoCopy(shared).Raw
 digest:=sha256.Sum256(snapshot);wantCCH:=fmt.Sprintf("%x",digest)[:5]
 a:=&auth.Auth{ID:"review237-native-key",Provider:"claude",ProxyURL:"direct",Attributes:map[string]string{"api_key":"synthetic-review237","base_url":server.URL}}
 e:=NewClaudeExecutor(&config.Config{});req:=exec.Request{Model:"claude-sonnet-4-6",Payload:payload};opts:=exec.Options{SourceFormat:translator.FormatClaude,Headers:http.Header{"User-Agent":[]string{"claude-cli/2.1.85 (external, cli)"}}}
 var first,firstSnapshot []byte;var wireAlias string
 for i:=0;i<2;i++{
  if _,err:=e.CountTokens(context.Background(),a,req,opts);err!=nil{t.Fatal(err)};wire:=<-captured
  if !bytes.Equal(req.Payload,snapshot)||!bytes.Equal(shared,snapshot)||alias!=string(snapshot){t.Fatal("Count mutated request/shared/no-copy snapshot")}
  if got:=cchFromBody(t,wire);got!=wantCCH{t.Fatalf("final CCH=%s want independent preimage SHA=%s",got,wantCCH)}
  if got:=billingVersionFromBody(t,wire);got!="2.1.85"{t.Fatalf("billing version=%s",got)}
  if i==0{first=wire;firstSnapshot=bytes.Clone(wire);wireAlias=util.ParseGJSONBytesNoCopy(first).Raw}else if !bytes.Equal(wire,firstSnapshot)||!bytes.Equal(first,firstSnapshot)||wireAlias!=string(firstSnapshot){t.Fatal("Count repeated call changed final wire or retained snapshot")}
 }
}
