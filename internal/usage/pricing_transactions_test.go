package usage

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestPricingConcurrentSavesAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	m := NewPricingCatalogManager()
	m.SetFetchers([]pricingFetcher{{ID: "fixture", Fetch: func(context.Context, *http.Client, string) (map[string]PricingModel, error) {
		return map[string]PricingModel{"fixture": {InputUSDPerMTok: 2, OutputUSDPerMTok: 3}}, nil
	}}})
	if err := m.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	price := PricingModel{InputUSDPerMTok: 2, OutputUSDPerMTok: 7}
	for i := 0; i < 20; i++ {
		if _, err := m.PutOverride(fmt.Sprintf("old%d", i), price); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			<-start
			if _, err := m.PutOverride(fmt.Sprintf("new%d", i), price); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			<-start
			if !m.DeleteOverride(fmt.Sprintf("old%d", i)) {
				t.Error("delete failed")
			}
		})
		wg.Go(func() {
			<-start
			if err := m.LoadFromPersistence(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Go(func() {
		<-start
		if err := m.RefreshOfficial(context.Background()); err != nil {
			t.Error(err)
		}
	})
	close(start)
	wg.Wait()
	after := m.Snapshot(nil)
	reopened := NewPricingCatalogManager()
	if err := reopened.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	disk := reopened.Snapshot(nil)
	if len(after.Overrides) != 20 || len(disk.Overrides) != 20 ||
		after.Models["fixture"].OutputUSDPerMTok != 3 || disk.Models["fixture"].OutputUSDPerMTok != 3 {
		t.Fatal("live and reopened catalogs differ")
	}
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("new%d", i)
		if after.Overrides[name].InputUSDPerMTok != 2 || disk.Overrides[name].InputUSDPerMTok != 2 {
			t.Fatal("successful override lost")
		}
	}
}

func TestPricingRefreshWriteFailurePreservesCatalog(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "catalog")
	path := filepath.Join(dir, "data.json")
	m := NewPricingCatalogManager()
	m.SetFetchers([]pricingFetcher{{ID: "fixture", Fetch: func(context.Context, *http.Client, string) (map[string]PricingModel, error) {
		return map[string]PricingModel{"fixture": {InputUSDPerMTok: 2, OutputUSDPerMTok: 3}}, nil
	}}})
	if err := m.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PutOverride("manual", PricingModel{InputUSDPerMTok: 4, OutputUSDPerMTok: 5}); err != nil {
		t.Fatal(err)
	}
	initial := m.Snapshot(nil)
	if err := os.Rename(dir, dir+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.RefreshOfficial(context.Background()); err == nil {
		t.Fatal("refresh returned success with blocked persistence")
	}
	if !reflect.DeepEqual(initial, m.Snapshot(nil)) {
		t.Fatal("failed refresh changed published catalog")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir+".saved", dir); err != nil {
		t.Fatal(err)
	}
	if err := m.RefreshOfficial(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := m.Snapshot(nil)
	if got.Models["fixture"].InputUSDPerMTok != 2 || got.Overrides["manual"].OutputUSDPerMTok != 5 {
		t.Fatal("successful refresh lost prices")
	}
	reopened := NewPricingCatalogManager()
	reopened.SetFetchers(m.fetchers)
	if err := reopened.SetPersistencePath(path); err != nil {
		t.Fatal(err)
	}
	disk := reopened.Snapshot(nil)
	if disk.Models["fixture"].InputUSDPerMTok != 2 || disk.Overrides["manual"].OutputUSDPerMTok != 5 || !reflect.DeepEqual(got.Official, disk.Official) {
		t.Fatal("successful refresh not persisted")
	}
}
