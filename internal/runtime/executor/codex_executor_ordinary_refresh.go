package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type ordinaryCodexGrantFunc func(context.Context, string) (*codexauth.CodexTokenData, error)
type ordinaryCodexGrantKey struct{}

func (e *CodexExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if auth == nil {
		return nil, statusErr{code: 400, msg: "missing Codex credential", requestScoped: true}
	}
	if auth.Disabled || auth.Status == cliproxyauth.StatusDisabled {
		return nil, statusErr{code: 403, msg: "Codex credential is disabled", requestScoped: true}
	}
	if auth.RefreshDisabled() || auth.AutoQuarantined {
		return auth, nil
	}
	if fresh, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return fresh, err
	}
	if strings.TrimSpace(auth.Attributes["api_key"]) != "" {
		return auth, nil
	}
	diskPath, err := resolveCodexAuthDiskPath(e.cfg, auth)
	if err != nil {
		return nil, err
	}
	grant, _ := ctx.Value(ordinaryCodexGrantKey{}).(ordinaryCodexGrantFunc)
	if grant == nil {
		client := e.ordinaryCodexHTTPClient(ctx, auth)
		client.Timeout = 30 * time.Second
		grant = codexauth.NewCodexAuthWithHTTPClient(client).RefreshTokens
	}
	if diskPath == "" {
		return refreshOrdinaryCodexGrant(ctx, auth, "", grant)
	}
	var fresh *cliproxyauth.Auth
	err = codexauth.WithFileLock(diskPath, func() error {
		var errRefresh error
		fresh, errRefresh = refreshOrdinaryCodexGrant(ctx, auth, diskPath, grant)
		return errRefresh
	})
	return fresh, err
}

func ordinaryCodexMetadataString(record map[string]any, key string) string {
	value, _ := record[key].(string)
	return strings.TrimSpace(value)
}

func readOrdinaryCodexCredential(path string) (map[string]any, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record map[string]any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err = decoder.Decode(&record); err != nil || record == nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
		return nil, fmt.Errorf("invalid Codex credential JSON")
	}
	return record, nil
}

func ordinaryCodexGrantChanged() error {
	return statusErr{code: http.StatusConflict, msg: "Codex credential grant changed while refreshing", requestScoped: true}
}

func refreshOrdinaryCodexGrant(ctx context.Context, auth *cliproxyauth.Auth, diskPath string, grant ordinaryCodexGrantFunc) (*cliproxyauth.Auth, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fresh := auth.Clone()
	if fresh.Metadata == nil {
		fresh.Metadata = make(map[string]any)
	}
	fresh.CodexRefreshPatch = nil
	token := ordinaryCodexMetadataString(auth.Metadata, "refresh_token")
	account := ordinaryCodexMetadataString(auth.Metadata, "account_id")
	var baseline map[string]any
	if diskPath != "" {
		var err error
		baseline, err = readOrdinaryCodexCredential(diskPath)
		if err != nil {
			return nil, err
		}
		accountDisk := ordinaryCodexMetadataString(baseline, "account_id")
		tokenDisk := ordinaryCodexMetadataString(baseline, "refresh_token")
		accessDisk := ordinaryCodexMetadataString(baseline, "access_token")
		access := ordinaryCodexMetadataString(auth.Metadata, "access_token")
		if account != "" && accountDisk != "" && accountDisk != account {
			return nil, ordinaryCodexGrantChanged()
		}
		if tokenDisk != token {
			return nil, ordinaryCodexGrantChanged()
		}
		disabled, _ := baseline["disabled"].(bool)
		stored := &cliproxyauth.Auth{Metadata: baseline}
		if disabled || stored.RefreshDisabled() {
			return nil, cliproxyauth.ErrSelectedRefreshUnavailable
		}
		if accessDisk != access {
			if accountDisk == "" || accountDisk != account || tokenDisk == "" || accessDisk == "" {
				return nil, ordinaryCodexGrantChanged()
			}
			fresh.Metadata = baseline
			return finishOrdinaryCodexRefresh(fresh, nil), nil
		}
	}
	if token == "" {
		return fresh, nil
	}
	next, err := grant(ctx, token)
	if err != nil {
		return nil, err
	}
	if next == nil || strings.TrimSpace(next.AccessToken) == "" {
		return nil, statusErr{code: 502, msg: "invalid Codex OAuth refresh response"}
	}
	if account != "" && strings.TrimSpace(next.AccountID) != "" && strings.TrimSpace(next.AccountID) != account {
		return nil, ordinaryCodexGrantChanged()
	}
	updates := map[string]string{"access_token": next.AccessToken, "refresh_token": next.RefreshToken, "id_token": next.IDToken, "account_id": next.AccountID, "email": next.Email, "expired": next.Expire, "plan_type": next.PlanType}
	for k, v := range updates {
		if strings.TrimSpace(v) != "" {
			fresh.Metadata[k] = v
		}
	}
	fresh.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	patch := codexauth.NewCredentialRefreshPatchWithDiskBaseline(auth.Metadata, baseline, fresh.Metadata, true, false)
	patch.SetUpstreamRefreshed(true)
	if diskPath != "" {
		latest, err := readOrdinaryCodexCredential(diskPath)
		if err != nil {
			return nil, err
		}
		if !patch.CanApplyTo(latest) || ordinaryCodexMetadataString(latest, "account_id") != ordinaryCodexMetadataString(baseline, "account_id") {
			return nil, ordinaryCodexGrantChanged()
		}
		merged := patch.Apply(latest)
		if err = codexauth.WriteJSONFileAtomic(diskPath, merged); err != nil {
			return nil, err
		}
		fresh.Metadata = merged
	}
	return finishOrdinaryCodexRefresh(fresh, patch), nil
}

func finishOrdinaryCodexRefresh(fresh *cliproxyauth.Auth, patch *codexauth.CredentialRefreshPatch) *cliproxyauth.Auth {
	fresh.CodexRefreshPatch = patch
	plan := ordinaryCodexMetadataString(fresh.Metadata, "plan_type")
	if plan != "" {
		if fresh.Attributes == nil {
			fresh.Attributes = make(map[string]string)
		}
		fresh.Attributes["plan_type"] = plan
	}
	if storage, ok := fresh.Storage.(*codexauth.CodexTokenStorage); ok && storage != nil {
		copied := *storage
		if plan != "" {
			copied.PlanType = plan
		}
		copied.RefreshPatch = patch
		fresh.Storage = &copied
	}
	return fresh
}
