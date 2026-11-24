package usage

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type pricingTestTransport func(*http.Request) (*http.Response, error)

func (f pricingTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestExplicitPricingRefreshUsesPublicServiceRequests(t *testing.T) {
	catalog := NewPricingCatalogManager()
	calls := map[string]int{}
	catalog.httpClient = &http.Client{Transport: pricingTestTransport(func(req *http.Request) (*http.Response, error) {
		address := req.URL.String()
		calls[address]++
		if req.Method != http.MethodGet || req.Body != nil || req.URL.User != nil ||
			req.URL.RawQuery != "" || req.UserAgent() != "CLIProxyAPI-Pricing/1.0" {
			t.Fatalf("unexpected public pricing request shape for %s", address)
		}
		for name := range req.Header {
			switch http.CanonicalHeaderKey(name) {
			case "User-Agent", "Accept":
			default:
				t.Fatalf("unexpected public pricing header %s", name)
			}
		}
		var body string
		switch address {
		case "https://developers.openai.com/api/docs/pricing":
			body = `[[0,"Fixture GPT"],[0,1],[0,0.5],[0,2]]`
		case "https://www.anthropic.com/pricing":
			body = `card_pricing_title_text>Fixture< data-value="1" data-value="2" data-value="3" data-value="4"`
		default:
			t.Fatalf("unexpected pricing endpoint: %s", address)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	_ = catalog.Snapshot(nil)
	if len(calls) != 0 {
		t.Fatal("local pricing read contacted a public source")
	}
	if err := catalog.RefreshOfficial(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, map[string]int{
		"https://developers.openai.com/api/docs/pricing": 1,
		"https://www.anthropic.com/pricing":              1,
	}) {
		t.Fatalf("unexpected public requests: %v", calls)
	}
	if len(catalog.officialModels) != 2 {
		t.Fatalf("expected both synthetic sources to refresh, got %d", len(catalog.officialModels))
	}
}
