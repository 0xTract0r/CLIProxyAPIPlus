package auth

import (
	"bytes"
	"encoding/json"
	"io"
	"math/big"
	"reflect"
	"strings"
)

// EqualCredentialJSON compares complete JSON values without rounding decimal
// numbers. Equivalent numeric spellings do not force credential file rewrites.
func EqualCredentialJSON(a, b []byte) bool {
	decode := func(raw []byte) (any, bool) {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var v, trailing any
		if d.Decode(&v) != nil || d.Decode(&trailing) != io.EOF {
			return nil, false
		}
		return canonicalCredentialJSON(v), true
	}
	x, ok := decode(a)
	if !ok {
		return false
	}
	y, ok := decode(b)
	return ok && reflect.DeepEqual(x, y)
}

type credentialDecimal struct {
	Negative         bool
	Digits, Exponent string
}

func canonicalCredentialJSON(value any) any {
	switch v := value.(type) {
	case json.Number:
		// Represent coefficient * 10^exponent symbolically: memory depends on the
		// input length, never the magnitude of an untrusted exponent.
		s := v.String()
		negative := strings.HasPrefix(s, "-")
		s = strings.TrimPrefix(s, "-")
		mantissa, exponent, hasExponent := strings.Cut(strings.ToLower(s), "e")
		var power big.Int
		if hasExponent {
			power.SetString(exponent, 10)
		}
		fraction := 0
		if i := strings.IndexByte(mantissa, '.'); i >= 0 {
			fraction = len(mantissa) - i - 1
			mantissa = mantissa[:i] + mantissa[i+1:]
		}
		digits := strings.TrimLeft(mantissa, "0")
		if digits == "" {
			return credentialDecimal{}
		}
		trimmed := strings.TrimRight(digits, "0")
		var adjustment big.Int
		adjustment.SetInt64(int64(len(digits) - len(trimmed) - fraction))
		power.Add(&power, &adjustment)
		return credentialDecimal{negative, trimmed, power.String()}
	case map[string]any:
		for k, item := range v {
			v[k] = canonicalCredentialJSON(item)
		}
	case []any:
		for i, item := range v {
			v[i] = canonicalCredentialJSON(item)
		}
	}
	return value
}
