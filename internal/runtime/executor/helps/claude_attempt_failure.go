package helps

import (
	"context"
	"errors"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// TrackClaudeAttemptFailure preserves the existing reporter contract except
// for local gate refusals. A rejected retry must report the real earlier
// transport failure; a request never sent upstream has no upstream failure.
func TrackClaudeAttemptFailure(ctx context.Context, reporter *UsageReporter, errPtr *error) {
	if reporter == nil {
		return
	}
	if errPtr != nil && *errPtr != nil {
		var gateErr *cliproxyexecutor.HTTPAttemptGateError
		if errors.As(*errPtr, &gateErr) {
			if gateErr.Previous != nil {
				reporter.PublishFailure(ctx, gateErr.Previous)
				return
			}
			if !gateErr.Sent {
				return
			}
		}
	}
	reporter.TrackFailure(ctx, errPtr)
}
