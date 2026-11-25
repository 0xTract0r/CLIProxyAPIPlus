package executor

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/httptransport"
	"golang.org/x/net/http/httpguts"
)

const (
	kiloGatewayBase       = "https://ai-gateway.kilo.ai/api/v1"
	kiloDeviceGatewayBase = "https://api.kilo.ai/api/gateway"
)

type kiloGatewayCredential struct {
	base         string
	token        string
	organization string
}

type KiloExecutor struct {
	cfg    *config.Config
	compat *OpenAICompatExecutor
}

func NewKiloExecutor(cfg *config.Config) *KiloExecutor {
	return &KiloExecutor{cfg: cfg, compat: NewOpenAICompatExecutor("kilo", cfg)}
}

func (e *KiloExecutor) Identifier() string { return "kilo" }

func (e *KiloExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil || req.URL == nil || req.Host != "" && req.Host != req.URL.Host {
		return ordinaryProviderStop(http.StatusBadRequest, "invalid Kilo Gateway request")
	}
	target := req.URL.String()
	list := req.Method == http.MethodGet && target == kiloGatewayBase+"/models"
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	for name := range req.Header {
		switch strings.ToLower(name) {
		case "authorization", "proxy-authorization", "cookie", "x-api-key", "x-goog-api-key", "x-kilocode-organizationid":
			delete(req.Header, name)
		}
	}
	if list {
		return nil
	}
	credential, err := kiloGatewayCredentials(auth)
	if err != nil {
		return err
	}
	if req.Method != http.MethodPost || target != credential.base+"/chat/completions" {
		return ordinaryProviderStop(http.StatusForbidden, "Kilo credentials are scoped to their assigned gateway")
	}
	req.Header.Set("Authorization", "Bearer "+credential.token)
	req.Header.Set("Content-Type", "application/json")
	if credential.organization != "" {
		req.Header.Set("X-KiloCode-OrganizationId", credential.organization)
	}
	return nil
}

func (e *KiloExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, ordinaryProviderStop(http.StatusBadRequest, "Kilo Gateway request is missing")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	req = req.Clone(ctx)
	if err := e.PrepareRequest(req, auth); err != nil {
		return nil, err
	}
	return ordinaryProviderClient(ctx, e.cfg, auth).Do(req)
}

func (e *KiloExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx, auth, credential, err := e.chatContext(ctx, auth, opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	resp, err := e.compat.executeWithCredentials(ctx, auth, req, opts, credential.base, credential.token, nil)
	return resp, preserveOrdinaryProviderRefusal(err)
}

func (e *KiloExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	ctx, auth, credential, err := e.chatContext(ctx, auth, opts)
	if err != nil {
		return nil, err
	}
	stream, err := e.compat.executeStreamWithCredentials(ctx, auth, req, opts, credential.base, credential.token, nil)
	if err != nil {
		return nil, preserveOrdinaryProviderRefusal(err)
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		for chunk := range stream.Chunks {
			if chunk.Err != nil {
				chunk.Err = preserveOrdinaryProviderRefusal(chunk.Err)
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: stream.Headers, Chunks: out}, nil
}

func (e *KiloExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.compat.CountTokens(ctx, auth, req, opts)
}

func (e *KiloExecutor) Refresh(_ context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}

func (e *KiloExecutor) chatContext(ctx context.Context, auth *cliproxyauth.Auth, opts cliproxyexecutor.Options) (context.Context, *cliproxyauth.Auth, kiloGatewayCredential, error) {
	if opts.Alt != "" {
		return ctx, nil, kiloGatewayCredential{}, ordinaryProviderStop(http.StatusNotImplemented, "the public Kilo Gateway supports chat completions")
	}
	credential, err := kiloGatewayCredentials(auth)
	if err != nil {
		return ctx, nil, kiloGatewayCredential{}, err
	}
	safeAuth := auth.Clone()
	for name := range safeAuth.Attributes {
		if strings.HasPrefix(strings.ToLower(name), "header:") {
			delete(safeAuth.Attributes, name)
		}
	}
	wrapped := httptransport.WithDecorator(ctx, func(next http.RoundTripper) http.RoundTripper {
		next = httptransport.Decorate(ctx, &http.Client{Transport: next}).Transport
		return kiloGatewayTransport{next: next, prepare: func(req *http.Request) error { return e.PrepareRequest(req, safeAuth) }}
	})
	return wrapped, safeAuth, credential, nil
}

type kiloGatewayTransport struct {
	next    http.RoundTripper
	prepare func(*http.Request) error
}

func (t kiloGatewayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if err := t.prepare(req); err != nil {
		return nil, err
	}
	resp, err := t.next.RoundTrip(req)
	if err == nil && resp != nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// Preserve the supplier's response and never follow its Location.
		cloned := *resp
		cloned.Header = resp.Header.Clone()
		cloned.Header.Del("Location")
		resp = &cloned
	}
	return resp, err
}

func kiloGatewayCredentials(auth *cliproxyauth.Auth) (kiloGatewayCredential, error) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "kilo") ||
		auth.Disabled || auth.Status == cliproxyauth.StatusDisabled {
		return kiloGatewayCredential{}, ordinaryProviderStop(http.StatusUnauthorized, "an enabled Kilo credential is required")
	}
	credential := kiloGatewayCredential{base: kiloGatewayBase}
	key, _ := auth.Metadata["api_key"].(string)
	if strings.TrimSpace(key) == "" {
		key = auth.Attributes["api_key"]
	}
	org, _ := auth.Metadata["organization_id"].(string)
	if strings.TrimSpace(key) == "" {
		// The vendor's published device integration binds its grant to this
		// gateway. A server rejection never changes the chosen endpoint.
		credential.base = kiloDeviceGatewayBase
		key, _ = auth.Metadata["kilocodeToken"].(string)
		org, _ = auth.Metadata["kilocodeOrganizationId"].(string)
	}
	credential.token, credential.organization = strings.TrimSpace(key), strings.TrimSpace(org)
	if credential.token == "" || !httpguts.ValidHeaderFieldValue(credential.token) {
		return kiloGatewayCredential{}, ordinaryProviderStop(http.StatusUnauthorized, "a Kilo API key or approved account login is required")
	}
	if !httpguts.ValidHeaderFieldValue(credential.organization) {
		return kiloGatewayCredential{}, ordinaryProviderStop(http.StatusBadRequest, "invalid Kilo organization scope")
	}
	return credential, nil
}
