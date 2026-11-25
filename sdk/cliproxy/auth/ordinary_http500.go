package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"reflect"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type ordinaryHTTP500Key struct{}

// This state belongs to one public execution. Only synchronous execution and
// stream bootstrap may authorize another round, before the caller sees data.
type ordinaryHTTP500Round struct {
	failed, required *ordinaryHTTP500Target
}
type ordinaryHTTP500Target struct {
	authID, provider, model string
	epoch, version          uint64
	body, original          [sha256.Size]byte
	path                    string
	headers                 http.Header
	cause                   error
}

func requiredHTTP500Target(ctx context.Context) *ordinaryHTTP500Target {
	if ctx == nil {
		return nil
	}
	round, _ := ctx.Value(ordinaryHTTP500Key{}).(*ordinaryHTTP500Round)
	if round == nil {
		return nil
	}
	return round.required
}

func recordHTTP500Target(ctx context.Context, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, err error) {
	if ctx == nil {
		return
	}
	round, _ := ctx.Value(ordinaryHTTP500Key{}).(*ordinaryHTTP500Round)
	if round == nil || auth == nil || auth.ID == "" || auth.RegistrationEpoch == 0 || auth.CredentialVersion == 0 || req.Model == "" ||
		statusCodeFromError(err) != http.StatusInternalServerError || !hasUpstreamExecutionAttempt(err) ||
		isRequestStopError(err) || isRequestInvalidError(err) || isCredentialTerminatedError(err) || isTOSViolationError(err) ||
		cliproxyexecutor.IsUpstreamWebsocketTurnError(err) {
		return
	}
	path, _ := opts.Metadata[cliproxyexecutor.RequestPathMetadataKey].(string)
	round.failed = &ordinaryHTTP500Target{
		authID: auth.ID, provider: provider, model: req.Model, epoch: auth.RegistrationEpoch, version: auth.CredentialVersion,
		body: sha256.Sum256(req.Payload), original: sha256.Sum256(opts.OriginalRequest), headers: opts.Headers.Clone(), path: path, cause: err,
	}
}

func (r *ordinaryHTTP500Round) nextPolicyError(err error, opts *cliproxyexecutor.Options, attempted map[string]struct{}) error {
	target := r.failed
	r.failed = nil
	var terminal, failed *upstreamExecutionAttemptError
	if target != nil && len(attempted) == 1 && errors.As(err, &terminal) && errors.As(target.cause, &failed) && terminal.attemptID == failed.attemptID {
		_, selected := attempted[target.authID]
		pin := pinnedAuthIDFromMetadata(opts.Metadata)
		if selected && (pin == "" || pin == target.authID) {
			r.required = target
			opts.Metadata = cloneRequestMetadata(opts.Metadata)
			opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey] = target.authID
			return target.cause
		}
	}
	if isOrdinaryUpstreamRefusal(err) || isTerminalRequestStopError(err) ||
		isSchedulerRejectError(err) || hasUpstreamExecutionAttempt(err) || len(attempted) != 0 {
		return nil
	}
	return err
}

func (r *ordinaryHTTP500Target) sameRegistration(auth *Auth, provider string) bool {
	return auth != nil && auth.ID == r.authID && auth.RegistrationEpoch == r.epoch && auth.CredentialVersion == r.version && provider == r.provider
}

// Re-run request preparation and authorization normally. Abort before the
// transport if reload or interception changed the effective destination.
func checkHTTP500Replay(ctx context.Context, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
	r := requiredHTTP500Target(ctx)
	if r == nil {
		return nil
	}
	path, _ := opts.Metadata[cliproxyexecutor.RequestPathMetadataKey].(string)
	if !r.sameRegistration(auth, provider) || req.Model != r.model || path != r.path ||
		sha256.Sum256(req.Payload) != r.body || sha256.Sum256(opts.OriginalRequest) != r.original || !reflect.DeepEqual(opts.Headers, r.headers) {
		return wrapRequestStopError(r.cause)
	}
	return nil
}
