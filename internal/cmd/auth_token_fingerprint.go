package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type AuthTokenFingerprintOptions struct {
	Paths     []string
	Provider  string
	Recursive bool
	Format    string
	NoHeader  bool
}

type AuthTokenFingerprintRecord struct {
	Path                 string `json:"path"`
	File                 string `json:"file"`
	ModTime              string `json:"mtime"`
	Size                 int64  `json:"size"`
	Type                 string `json:"type"`
	Email                string `json:"email"`
	AccountID            string `json:"account_id"`
	HasAccessToken       bool   `json:"has_access_token"`
	AccessTokenSHA256_16 string `json:"access_token_sha256_16,omitempty"`
	AccessTokenLength    int    `json:"access_token_length,omitempty"`
	HasRefreshToken      bool   `json:"has_refresh_token"`
	RefreshTokenSHA25616 string `json:"refresh_token_sha256_16,omitempty"`
	RefreshTokenLength   int    `json:"refresh_token_length,omitempty"`
	LastRefresh          string `json:"last_refresh"`
	Expired              string `json:"expired"`
	RefreshDisabled      string `json:"refresh_disabled"`
	RefreshEnabled       string `json:"refresh_enabled"`
	ReauthRequired       string `json:"reauth_required"`
	RefreshStatus        string `json:"refresh_status"`
	RefreshErrorCode     string `json:"refresh_error_code"`
}

func RunAuthTokenFingerprint(ctx context.Context, out io.Writer, opts AuthTokenFingerprintOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if out == nil {
		out = io.Discard
	}
	if len(opts.Paths) == 0 {
		return errors.New("at least one --path is required")
	}
	format := strings.ToLower(strings.TrimSpace(opts.Format))
	if format == "" {
		format = "tsv"
	}
	if format != "tsv" && format != "jsonl" {
		return fmt.Errorf("unsupported format %q: use tsv or jsonl", opts.Format)
	}
	provider := strings.ToLower(strings.TrimSpace(opts.Provider))

	records, err := collectAuthTokenFingerprints(ctx, opts.Paths, provider, opts.Recursive)
	if err != nil {
		return err
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Path == records[j].Path {
			return records[i].File < records[j].File
		}
		return records[i].Path < records[j].Path
	})

	switch format {
	case "jsonl":
		enc := json.NewEncoder(out)
		for _, record := range records {
			if errEncode := enc.Encode(record); errEncode != nil {
				return errEncode
			}
		}
	default:
		if !opts.NoHeader {
			if _, errWrite := fmt.Fprintln(out, strings.Join(authTokenFingerprintHeader(), "\t")); errWrite != nil {
				return errWrite
			}
		}
		for _, record := range records {
			if _, errWrite := fmt.Fprintln(out, strings.Join(record.tsvFields(), "\t")); errWrite != nil {
				return errWrite
			}
		}
	}
	return nil
}

func collectAuthTokenFingerprints(ctx context.Context, paths []string, provider string, recursive bool) ([]AuthTokenFingerprintRecord, error) {
	var records []AuthTokenFingerprintRecord
	for _, rawPath := range paths {
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, errCtx
		}
		path := strings.TrimSpace(rawPath)
		if path == "" {
			continue
		}
		info, errStat := os.Stat(path)
		if errStat != nil {
			return nil, fmt.Errorf("stat %s: %w", path, errStat)
		}
		if !info.IsDir() {
			record, ok, errInspect := inspectAuthTokenFile(path, provider)
			if errInspect != nil {
				return nil, errInspect
			}
			if ok {
				records = append(records, record)
			}
			continue
		}
		if recursive {
			errWalk := filepath.WalkDir(path, func(candidate string, entry fs.DirEntry, errWalk error) error {
				if errWalk != nil {
					return errWalk
				}
				if errCtx := ctx.Err(); errCtx != nil {
					return errCtx
				}
				if entry.IsDir() {
					return nil
				}
				record, ok, errInspect := inspectAuthTokenFile(candidate, provider)
				if errInspect != nil {
					return errInspect
				}
				if ok {
					records = append(records, record)
				}
				return nil
			})
			if errWalk != nil {
				return nil, errWalk
			}
			continue
		}
		entries, errRead := os.ReadDir(path)
		if errRead != nil {
			return nil, fmt.Errorf("read dir %s: %w", path, errRead)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			record, ok, errInspect := inspectAuthTokenFile(filepath.Join(path, entry.Name()), provider)
			if errInspect != nil {
				return nil, errInspect
			}
			if ok {
				records = append(records, record)
			}
		}
	}
	return records, nil
}

func inspectAuthTokenFile(path string, provider string) (AuthTokenFingerprintRecord, bool, error) {
	var empty AuthTokenFingerprintRecord
	if !strings.EqualFold(filepath.Ext(path), ".json") {
		return empty, false, nil
	}
	if provider != "" && !strings.HasPrefix(strings.ToLower(filepath.Base(path)), provider+"-") {
		return empty, false, nil
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		return empty, false, fmt.Errorf("stat %s: %w", path, errStat)
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return empty, false, fmt.Errorf("read %s: %w", path, errRead)
	}
	var raw map[string]any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if errDecode := dec.Decode(&raw); errDecode != nil {
		return empty, false, fmt.Errorf("decode %s: %w", path, errDecode)
	}
	flat := flattenAuthTokenMap(raw)
	tokenType := firstString(flat, "type", "provider")
	if provider != "" {
		if tokenType != "" && !strings.EqualFold(tokenType, provider) {
			return empty, false, nil
		}
	}
	accessToken := firstString(flat, "access_token", "accessToken")
	refreshToken := firstString(flat, "refresh_token", "refreshToken")
	return AuthTokenFingerprintRecord{
		Path:                 path,
		File:                 filepath.Base(path),
		ModTime:              info.ModTime().UTC().Format(time.RFC3339Nano),
		Size:                 info.Size(),
		Type:                 tokenType,
		Email:                firstString(flat, "email"),
		AccountID:            firstString(flat, "account_id", "accountID", "accountId", "id"),
		HasAccessToken:       accessToken != "",
		AccessTokenSHA256_16: shortTokenFingerprint(accessToken),
		AccessTokenLength:    len(accessToken),
		HasRefreshToken:      refreshToken != "",
		RefreshTokenSHA25616: shortTokenFingerprint(refreshToken),
		RefreshTokenLength:   len(refreshToken),
		LastRefresh:          firstString(flat, "last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"),
		Expired:              firstString(flat, "expired", "expires_at", "expiresAt", "expire"),
		RefreshDisabled:      firstValueString(flat, "refresh_disabled", "disable_refresh", "auto_refresh_disabled", "account_settings.refresh_disabled", "account_settings.disable_refresh", "account_settings.auto_refresh_disabled"),
		RefreshEnabled:       firstValueString(flat, "refresh_enabled", "auto_refresh", "auto_refresh_enabled", "account_settings.refresh_enabled", "account_settings.auto_refresh", "account_settings.auto_refresh_enabled"),
		ReauthRequired:       firstValueString(flat, "reauth_required", "account_settings.reauth_required"),
		RefreshStatus:        firstString(flat, "refresh_status", "account_settings.refresh_status"),
		RefreshErrorCode:     firstString(flat, "refresh_error_code", "account_settings.refresh_error_code"),
	}, true, nil
}

func flattenAuthTokenMap(raw map[string]any) map[string]any {
	out := make(map[string]any)
	var walk func(prefix string, value any)
	walk = func(prefix string, value any) {
		if prefix != "" {
			out[prefix] = value
		}
		obj, ok := value.(map[string]any)
		if !ok {
			return
		}
		for key, nested := range obj {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			next := key
			if prefix != "" {
				next = prefix + "." + key
			}
			walk(next, nested)
		}
	}
	walk("", raw)
	return out
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringFromAny(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func firstValueString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if raw, ok := values[key]; ok {
			return valueString(raw)
		}
	}
	return ""
}

func stringFromAny(raw any) string {
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case json.Number:
		return value.String()
	default:
		return ""
	}
}

func valueString(raw any) string {
	switch value := raw.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(value)
	case bool:
		return strconv.FormatBool(value)
	case json.Number:
		return value.String()
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case int:
		return strconv.Itoa(value)
	default:
		return fmt.Sprintf("%v", value)
	}
}

func shortTokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:16]
}

func authTokenFingerprintHeader() []string {
	return []string{
		"path",
		"file",
		"mtime",
		"size",
		"type",
		"email",
		"account_id",
		"has_access_token",
		"access_token_sha256_16",
		"access_token_length",
		"has_refresh_token",
		"refresh_token_sha256_16",
		"refresh_token_length",
		"last_refresh",
		"expired",
		"refresh_disabled",
		"refresh_enabled",
		"reauth_required",
		"refresh_status",
		"refresh_error_code",
	}
}

func (r AuthTokenFingerprintRecord) tsvFields() []string {
	return []string{
		r.Path,
		r.File,
		r.ModTime,
		strconv.FormatInt(r.Size, 10),
		r.Type,
		r.Email,
		r.AccountID,
		strconv.FormatBool(r.HasAccessToken),
		r.AccessTokenSHA256_16,
		strconv.Itoa(r.AccessTokenLength),
		strconv.FormatBool(r.HasRefreshToken),
		r.RefreshTokenSHA25616,
		strconv.Itoa(r.RefreshTokenLength),
		r.LastRefresh,
		r.Expired,
		r.RefreshDisabled,
		r.RefreshEnabled,
		r.ReauthRequired,
		r.RefreshStatus,
		r.RefreshErrorCode,
	}
}
