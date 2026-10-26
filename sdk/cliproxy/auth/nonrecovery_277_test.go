package auth
import("context";"errors";"testing")
func TestNonRecovery277StoreIdentityAndFailedSaveIsolation(t *testing.T) {
 t.Run("identity-backfill",func(t *testing.T){
  m,_,_,old:=setup274(t)
  got,err:=m.CommitNonRecoveryUpdate(context.Background(),old.ID,old.RegistrationEpoch,old.CredentialVersion,func(a *Auth)(*Auth,error){a.Metadata["note"]="accepted-note";return a,nil},func(_ context.Context,a *Auth)error{a.Metadata["account_id"]="foreign-account";a.Metadata["email"]="foreign@example.invalid";return nil})
  live,_:=m.GetByID(old.ID)
  if err==nil && (got.Metadata["account_id"]!="account274" || live.Metadata["account_id"]!="account274") {t.Errorf("Store bypassed identity fence: returned=%v live=%v",got.Metadata["account_id"],live.Metadata["account_id"])}
 })
 t.Run("failed-save-error-pointer",func(t *testing.T){
  m,_,_,old:=setup274(t)
  m.mu.Lock();m.auths[old.ID].LastError=&Error{Message:"original-terminal",HTTPStatus:401};m.mu.Unlock()
  sentinel:=errors.New("atomic save failed")
  _,err:=m.CommitNonRecoveryUpdate(context.Background(),old.ID,old.RegistrationEpoch,old.CredentialVersion,func(a *Auth)(*Auth,error){a.Metadata["note"]="uncommitted-note";return a,nil},func(_ context.Context,a *Auth)error{a.LastError.Message="store-mutated-terminal";return sentinel})
  live,_:=m.GetByID(old.ID)
  if !errors.Is(err,sentinel){t.Fatalf("missing save error: %v",err)}
  if live.LastError.Message!="original-terminal" {t.Errorf("failed Save changed manager-owned error via alias: %q",live.LastError.Message)}
 })
}
