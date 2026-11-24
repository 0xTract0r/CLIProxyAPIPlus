package cliproxy

import (
	"errors"
	"reflect"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestWatcherWrapperForwardsRefreshAndScopedSnapshots(t *testing.T) {
	var absent *WatcherWrapper
	absent.NotifyTokenRefreshed("file", "new", "refresh", "later")
	(&WatcherWrapper{}).NotifyTokenRefreshed("file", "new", "refresh", "later")
	var received []string
	var lookup string
	record := &coreauth.Auth{ID: "fixture-account"}
	w := &WatcherWrapper{
		notifyTokenRefreshed: func(id, access, refresh, expires string) {
			received = []string{id, access, refresh, expires}
		},
		snapshotAuthFile: func(path string) ([]*coreauth.Auth, error) {
			lookup = path
			return []*coreauth.Auth{record}, nil
		},
	}
	w.NotifyTokenRefreshed("fixture", "sample-access", "sample-refresh", "tomorrow")
	if !reflect.DeepEqual(received, []string{"fixture", "sample-access", "sample-refresh", "tomorrow"}) {
		t.Fatal("refresh callback did not receive the original fields")
	}
	got, err := w.SnapshotAuthFile("one.json")
	if err != nil || lookup != "one.json" || len(got) != 1 || got[0] != record {
		t.Fatal("the upstream per-file snapshot callback was not preserved")
	}
	want := errors.New("synthetic snapshot failure")
	w.snapshotAuthFile = func(string) ([]*coreauth.Auth, error) { return nil, want }
	if _, err := w.SnapshotAuthFile("one.json"); !errors.Is(err, want) {
		t.Fatal("snapshot errors must reach the caller")
	}
}
