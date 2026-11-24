package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrSelectedRefreshChanged reports that the originally selected grant no longer owns the file.
	ErrSelectedRefreshChanged = errors.New("selected credential changed during refresh")
	// ErrSelectedRefreshUnavailable identifies an entry that cannot be refreshed in place.
	ErrSelectedRefreshUnavailable = errors.New("selected credential does not have an enabled refresh grant")
)

type selectedForceRefreshKey struct{}

type selectedRefreshWait struct{ until time.Time }

func (e selectedRefreshWait) Error() string {
	return "credential refresh must wait for the upstream retry window"
}

func (e selectedRefreshWait) StatusCode() int { return http.StatusTooManyRequests }

func (e selectedRefreshWait) Headers() http.Header {
	return http.Header{"Retry-After": {e.until.UTC().Format(http.TimeFormat)}}
}

// ForceRefreshSelectedAuth refreshes exactly the observed local grant. It uses
// the same refresh and persistence path as ForceRefreshAuth with a version fence.
func (m *Manager) ForceRefreshSelectedAuth(ctx context.Context, observed *Auth) (*Auth, error) {
	if m == nil || observed == nil || observed.ID == "" || observed.RegistrationEpoch == 0 || observed.CredentialVersion == 0 {
		return nil, ErrSelectedRefreshChanged
	}
	ctx = context.WithValue(WithForceRefresh(ctx), selectedForceRefreshKey{}, true)
	ctx = context.WithValue(WithoutSkipPersist(ctx), checkedStoreWriteKey{}, true)
	refreshed, err := m.refreshAuthForRequestAtVersion(ctx, observed.ID, "", observed.RegistrationEpoch, observed.CredentialVersion)
	if errors.Is(err, errVerifyCredentialRemoved) || errors.Is(err, errVerifyCredentialSuperseded) {
		return nil, ErrSelectedRefreshChanged
	}
	return refreshed, err
}

// HasSingleRefreshGrant reports whether the entry contains a single ordinary refresh grant.
func (a *Auth) HasSingleRefreshGrant() bool { return a != nil && hasOrdinaryRefreshGrant(a) }

func selectedUpstreamWaitUntil(err error, now time.Time) time.Time {
	if statusCodeFromError(err) != http.StatusTooManyRequests {
		return time.Time{}
	}
	var withHeaders interface{ Headers() http.Header }
	if errors.As(err, &withHeaders) {
		raw := strings.TrimSpace(withHeaders.Headers().Get("Retry-After"))
		if seconds, parseErr := strconv.ParseInt(raw, 10, 32); parseErr == nil && seconds >= 0 {
			return now.Add(time.Duration(seconds) * time.Second)
		}
		if when, parseErr := http.ParseTime(raw); parseErr == nil {
			return when
		}
	}
	if delay := trustedRetryAfterHint(err); delay != nil && *delay > 0 {
		return now.Add(*delay)
	}
	return time.Time{}
}

// SelectedCredentialAvailability evaluates the normal local availability
// checks for one observed credential. It does not select or rotate credentials.
func SelectedCredentialAvailability(auth *Auth, model string, now time.Time) (allowed bool, until time.Time) {
	blocked, _, next := isAuthBlockedForModel(auth, model, now)
	return !blocked, next
}
