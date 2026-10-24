package management
import("context";"os";"path/filepath";"reflect";"testing"
 codex "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex/credential"
 fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
 coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
 "github.com/router-for-me/CLIProxyAPI/v8/sdk/config")
func TestStorage293RealCodexConsumer(t *testing.T){
 for _,fail:=range []bool{false,true}{t.Run(map[bool]string{false:"success-backfill",true:"real-save-failure"}[fail],func(t *testing.T){
 ctx:=context.Background();dir:=t.TempDir();path:=filepath.Join(dir,"storage293.json")
 store:=&recoveryCountingFileStore{FileTokenStore:fileauth.NewFileTokenStore()};store.SetBaseDir(dir)
 ts:=&codex.CodexTokenStorage{Type:"original-type",AccessToken:"old-access",RefreshToken:"old-refresh",AccountID:"same",Email:"same@example.invalid",Metadata:map[string]any{"opaque":"original"},SIWC:map[string]any{"refresh_token":"old-siwc"}}
 storageBefore:=*ts; runtime:=map[string]string{"sentinel":"original"}
 m:=coreauth.NewManager(store,nil,nil)
 old,err:=m.Register(coreauth.WithSkipPersist(ctx),&coreauth.Auth{ID:"storage293.json",FileName:"storage293.json",Provider:"codex",ProxyURL:"direct",Attributes:map[string]string{coreauth.AttributePath:path},Storage:ts,Runtime:runtime,LastError:&coreauth.Error{Message:"original-terminal",HTTPStatus:401},AutoQuarantined:true,Metadata:map[string]any{"type":"codex","account_id":"same","access_token":"old-access","refresh_token":"old-refresh","note":"before","reauth_required":true}})
 if err!=nil{t.Fatal(err)}
 raw:=[]byte(`{"type":"codex","account_id":"same","access_token":"old-access","refresh_token":"old-refresh","note":"before"}`);if fail{raw=[]byte(`{invalid`)}
 if err=os.WriteFile(path,raw,0600);err!=nil{t.Fatal(err)}
 h:=NewHandlerWithoutConfigFilePath(&config.Config{AuthDir:dir},m);h.tokenStore=store;hooks:=0
 h.SetPostAuthPersistHook(func(context.Context,*coreauth.Auth)error{hooks++;return nil})
 incoming:=&coreauth.Auth{ID:old.ID,FileName:old.FileName,Provider:"codex",Metadata:map[string]any{"type":"codex","account_id":"same","access_token":"new-access","note":"after"}}
 store.saves=0;_,err=h.saveTokenRecord(ctx,incoming);live,_:=m.GetByID(old.ID);disk,_:=os.ReadFile(path)
 if !reflect.DeepEqual(*ts,storageBefore)||!reflect.DeepEqual(runtime,map[string]string{"sentinel":"original"}){t.Error("original Storage/Runtime mutated")}
 if store.saves!=1{t.Errorf("saves=%d",store.saves)}
 if fail{if err==nil||hooks!=0||string(disk)!=string(raw)||live.Metadata["note"]!="before"||live.CredentialVersion!=old.CredentialVersion||live.RegistrationEpoch!=old.RegistrationEpoch||live.LastError.Message!=old.LastError.Message||live.AutoQuarantined!=old.AutoQuarantined{t.Fatalf("failed Save mutated state err=%v hooks=%d",err,hooks)}} else {if err!=nil||hooks!=1||live.Metadata["note"]!="after"||live.Metadata["email"]!="same@example.invalid"||live.Metadata["access_token"]!="new-access"||live.RegistrationEpoch!=old.RegistrationEpoch||live.AutoQuarantined!=old.AutoQuarantined{t.Fatalf("legal backfill failed err=%v hooks=%d metadata=%v",err,hooks,live.Metadata)}}
 })}
}
