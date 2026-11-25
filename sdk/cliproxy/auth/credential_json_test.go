package auth

import "testing"

func TestCredentialJSONExactDecimalEquality(t *testing.T) {
	cases := []struct {
		name, a, b string
		equal      bool
	}{
		{"adjacent", `{"n":9007199254740992}`, `{"n":9007199254740993}`, false},
		{"nested", `{"a":[{"n":9007199254740997}]}`, `{"a":[{"n":9007199254740996}]}`, false},
		{"equivalent", `{"a":[1.00,-0,100e-2],"b":true}`, ` { "b": true, "a": [1e0,0.000,1] } `, true},
		{"scientific exact", `9007199254740993`, `9.007199254740993e15`, true},
		{"negative", `-100.0`, `-1e2`, true},
		{"sign", `-1`, `1`, false},
		{"tiny", `1e-999999999999999999999999`, `10e-1000000000000000000000000`, true},
		{"huge", `1e999999999999999999999999`, `10e999999999999999999999998`, true},
		{"huge distinct", `1e999999999999999999999999`, `1e999999999999999999999998`, false},
		{"zero huge", `-0e999999999999999999999999`, `0`, true},
		{"trailing object", `{"n":1} {}`, `{"n":1}`, false},
		{"trailing junk", `{"n":1}junk`, `{"n":1}`, false},
		{"invalid", `01`, `1`, false},
		{"types", `"1"`, `1`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EqualCredentialJSON([]byte(c.a), []byte(c.b)); got != c.equal {
				t.Fatalf("equality=%v want %v", got, c.equal)
			}
		})
	}
}
