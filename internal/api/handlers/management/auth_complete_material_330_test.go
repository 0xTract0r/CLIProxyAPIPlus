package management
import (
 "context"
 "encoding/base64"
 "os"
 "path/filepath"
 "sync/atomic"
 "testing"
 "time"
 "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
 fileauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
 coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)
type review330Store struct { *fileauth.FileTokenStore; armed bool; saves atomic.Int32; entered, release chan struct{} }
func(s *review330Store) Save(c context.Context,a *coreauth.Auth)(string,error){
 if s.armed { s.saves.Add(1); if s.entered!=nil {close(s.entered); <-s.release} }; return s.FileTokenStore.Save(c,a)
}
func TestReview330(t *testing.T){
 for _,mode:=range []string{"access-email-vs-prior","store-boundary"} {t.Run(mode,func(t *testing.T){
 c:=context.Background(); s:=&review330Store{FileTokenStore:fileauth.NewFileTokenStore()};s.SetBaseDir(t.TempDir());m:=coreauth.NewManager(s,nil,nil)
 old,e:=m.Register(coreauth.WithAuthCreationIntent(c),&coreauth.Auth{ID:"review330.json",FileName:"review330.json",Provider:"codex",Status:coreauth.StatusQuarantined,AutoQuarantined:true,Metadata:map[string]any{"type":"codex","account_id":"account","email":"old@example.invalid","proxy_url":"direct","access_token":"old-a","refresh_token":"old-r","reauth_required":true,"refresh_disabled":true,"refresh_disabled_reason":"reauth_required","siwc":map[string]any{"access_token":"old-sa","refresh_token":"old-sr"}}});if e!=nil {t.Fatal(e)}
 path:=old.Attributes[coreauth.AttributePath];before,e:=os.ReadFile(path);if e!=nil {t.Fatal(e)}
 h:=NewHandlerWithoutConfigFilePath(&config.Config{AuthDir:filepath.Dir(path)},m);h.tokenStore=s;var hooks atomic.Int32
 h.SetPostAuthPersistHook(func(c context.Context,a *coreauth.Auth)error{hooks.Add(1);return h.upsertAuthRecord(coreauth.WithSkipPersist(c),a)})
 in:=&coreauth.Auth{ID:old.ID,FileName:old.FileName,Provider:"codex",Metadata:map[string]any{"type":"codex","account_id":"account","access_token":"new-a","refresh_token":"new-r","siwc":map[string]any{"access_token":"new-sa","refresh_token":"new-sr"}}};s.armed=true
 if mode=="access-email-vs-prior" {
 in.Metadata["access_token"]="e30."+base64.RawURLEncoding.EncodeToString([]byte(`{"email":"other@example.invalid","https://api.openai.com/auth":{"chatgpt_account_id":"account"}}`))+"."
 _,e=h.saveTokenRecord(c,in);live,_:=m.GetByID(old.ID);after,_:=os.ReadFile(path)
 if e==nil || s.saves.Load()!=0 || hooks.Load()!=0 || live.RegistrationEpoch!=old.RegistrationEpoch || live.CredentialVersion!=old.CredentialVersion || string(before)!=string(after) {t.Fatalf("identity conflict accepted: err=%v Save=%d hook=%d epoch=%d->%d version=%d->%d diskChanged=%v",e,s.saves.Load(),hooks.Load(),old.RegistrationEpoch,live.RegistrationEpoch,old.CredentialVersion,live.CredentialVersion,string(before)!=string(after))};return
 }
 s.entered=make(chan struct{});s.release=make(chan struct{});done:=make(chan error,1)
 go func(){_,e:=h.saveTokenRecord(c,in);done<-e}()
 select{case <-s.entered:case <-time.After(5*time.Second):t.Fatal("Save not reached")}
 live,_:=m.GetByID(old.ID);middle,_:=os.ReadFile(path)
 if live.RegistrationEpoch!=old.RegistrationEpoch || live.CredentialVersion!=old.CredentialVersion || !live.AutoQuarantined || hooks.Load()!=0 || string(before)!=string(middle){t.Error("published before Save completed")}
 stale:=old.Clone();stale.Metadata["access_token"]="late-old";staleDone:=make(chan error,1)
 go func(){_,e:=m.UpdateRefreshedAuth(c,old,stale);staleDone<-e}();close(s.release)
 select{case e:=<-done:if e!=nil{t.Fatal(e)};case <-time.After(5*time.Second):t.Fatal("hook manager reentry deadlock")}
 select{case e:=<-staleDone:if e==nil{t.Fatal("stale refresh accepted")};case <-time.After(5*time.Second):t.Fatal("stale refresh blocked")}
 live,_=m.GetByID(old.ID);after,_:=os.ReadFile(path)
 if s.saves.Load()!=1 || hooks.Load()!=1 || live.RegistrationEpoch<=old.RegistrationEpoch || live.CredentialVersion<=old.CredentialVersion || live.AutoQuarantined || live.Metadata["access_token"]!="new-a" || string(before)==string(after){t.Fatalf("bad success Save=%d hook=%d epoch=%d->%d",s.saves.Load(),hooks.Load(),old.RegistrationEpoch,live.RegistrationEpoch)}
 t.Logf("Save=1 hook=1 epoch=%d->%d version=%d->%d; before Save old disk/runtime; hook reentered manager; concurrent old refresh rejected",old.RegistrationEpoch,live.RegistrationEpoch,old.CredentialVersion,live.CredentialVersion)
 })}
}


func TestReview337GateRechecksHookIdentityWithoutSideEffects(t *testing.T){
 if codexRecoveryTokenClaimsConsistent(map[string]any{"account_id":"account"},map[string]any{"account_id":"account","chatgpt_account_id":"different"}){t.Fatal("prior alias contradiction ignored")}
 c:=context.Background();s:=&review330Store{FileTokenStore:fileauth.NewFileTokenStore()};s.SetBaseDir(t.TempDir());m:=coreauth.NewManager(s,nil,nil)
 old,e:=m.Register(coreauth.WithAuthCreationIntent(c),&coreauth.Auth{ID:"review337.json",FileName:"review337.json",Provider:"codex",Disabled:true,AutoQuarantined:true,Status:coreauth.StatusQuarantined,Metadata:map[string]any{"type":"codex","account_id":"account","email":"old@example.invalid","proxy_url":"direct","access_token":"old-a","refresh_token":"old-r","reauth_required":true,"siwc":map[string]any{"access_token":"old-sa","refresh_token":"old-sr"}}});if e!=nil{t.Fatal(e)}
 path:=old.Attributes[coreauth.AttributePath];before,e:=os.ReadFile(path);if e!=nil{t.Fatal(e)}
 h:=NewHandlerWithoutConfigFilePath(&config.Config{AuthDir:filepath.Dir(path)},m);h.tokenStore=s;var hooks atomic.Int32;h.postAuthPersistHook=func(context.Context,*coreauth.Auth)error{hooks.Add(1);return nil}
 in:=&coreauth.Auth{ID:old.ID,FileName:old.FileName,Provider:"codex",Metadata:map[string]any{"type":"codex","account_id":"account","access_token":"new-a","refresh_token":"new-r","siwc":map[string]any{"access_token":"new-sa","refresh_token":"new-sr"}}}
 if !completeCodexSavedRecoveryMaterial(in,old){t.Fatal("valid pre-hook saved material not eligible")}
 called:=false;h.postAuthHook=func(_ context.Context,request *coreauth.Auth)error{called=true;request.Metadata["access_token"]="e30."+base64.RawURLEncoding.EncodeToString([]byte(`{"email":"other@example.invalid","https://api.openai.com/auth":{"chatgpt_account_id":"account"}}`))+".";return nil}
 s.armed=true;_,e=h.saveTokenRecord(c,in);live,_:=m.GetByID(old.ID);after,readErr:=os.ReadFile(path)
 if !called||e==nil||readErr!=nil||s.saves.Load()!=0||hooks.Load()!=0||string(before)!=string(after)||live.RegistrationEpoch!=old.RegistrationEpoch||live.CredentialVersion!=old.CredentialVersion||!live.Disabled||!live.AutoQuarantined{t.Fatalf("gate recheck failed: hookReached=%v rejected=%v Save=%d persistHook=%d unchangedDisk=%v",called,e!=nil,s.saves.Load(),hooks.Load(),string(before)==string(after))}
 if _,exists:=in.Metadata["email"];exists{t.Fatal("prior email supplemented incoming")}
}
