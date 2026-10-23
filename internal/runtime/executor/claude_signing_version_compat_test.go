package executor

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	xxHash64 "github.com/pierrec/xxHash/xxHash64"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This independent oracle is deliberately limited to these wire fixtures,
// whose model/dispatch fields are at the root. It does not call the production
// scanner or signer. Nested and raw-byte normalization have frozen U vectors.
func expectedCCH21220Fixture(t *testing.T, body []byte) string {
	t.Helper()
	header := gjson.GetBytes(body, "system.0.text").String()
	zeroHeader := claudeBillingHeaderCCHPattern.ReplaceAllString(header, "cch=00000;")
	view, err := sjson.SetBytes(body, "system.0.text", zeroHeader)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(view, "model").Type == gjson.String {
		view, err = sjson.SetBytes(view, "model", "")
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"max_tokens", "fallbacks", "fallback_credit_token"} {
		view, err = sjson.DeleteBytes(view, key)
		if err != nil {
			t.Fatal(err)
		}
	}
	return fmt.Sprintf("%05x", xxHash64.Checksum(view, 0x4D659218E32A3268)&0xFFFFF)
}

func TestClaudeCCHVersionSelectionUsesFinalHeaderAndRetryBytes(t *testing.T) {
	if got := expectedCCH21220Fixture(t, []byte(claudeCCH21220BaseBody)); got != "7ee87" {
		t.Fatalf("independent 220 oracle lost fixed upstream vector: %s", got)
	}
	for _, tc := range []struct {
		name, ua string
		modern   bool
	}{
		{"default211", "claude-cli/2.1.211 (external, cli)", false},
		{"below220", "claude-cli/2.1.219 (external, cli)", false},
		{"220", "claude-cli/2.1.220 (external, cli)", true},
		{"280", "claude-cli/2.1.280 (external, cli)", true},
		{"unknown", "synthetic-caller/1.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(claudeCCH21220BaseBody)
			// A caller-controlled billing version cannot select the CCH algorithm.
			if tc.modern {
				body = []byte(strings.Replace(string(body), "cc_version=2.1.220.test", "cc_version=2.1.211.test", 1))
			}
			req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("User-Agent", tc.ua)
			req.Header.Set("Content-Length", fmt.Sprint(len(body)))
			signed, err := signClaudeHTTPRequestCCH(req, body)
			if err != nil {
				t.Fatal(err)
			}
			want := recomputeExpectedCCH(t, signed)
			if tc.modern {
				want = expectedCCH21220Fixture(t, signed)
			}
			if got := cchFromBody(t, signed); got != want {
				t.Fatalf("CCH=%s want %s for %s", got, want, tc.ua)
			}
			sent, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			retry, err := req.GetBody()
			if err != nil {
				t.Fatal(err)
			}
			defer retry.Close()
			resent, err := io.ReadAll(retry)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(signed, sent) || !bytes.Equal(signed, resent) || req.ContentLength != int64(len(signed)) || req.Header.Get("Content-Length") != fmt.Sprint(len(signed)) {
				t.Fatal("returned/log bytes, request, retry or length diverged")
			}
			resigned, err := signClaudeHTTPRequestCCH(req, signed)
			if err != nil || !bytes.Equal(signed, resigned) {
				t.Fatal("same final header/body signing is not idempotent")
			}
		})
	}
}
