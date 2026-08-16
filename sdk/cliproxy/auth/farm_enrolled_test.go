package auth

import "testing"

func TestAuthFarmEnrolled(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want bool
	}{
		{name: "nil auth", auth: nil, want: false},
		{name: "nil metadata", auth: &Auth{}, want: false},
		{name: "missing key", auth: &Auth{Metadata: map[string]any{"other": true}}, want: false},
		{name: "bool true", auth: &Auth{Metadata: map[string]any{FarmEnrolledMetadataKey: true}}, want: true},
		{name: "bool false", auth: &Auth{Metadata: map[string]any{FarmEnrolledMetadataKey: false}}, want: false},
		{name: "string true", auth: &Auth{Metadata: map[string]any{FarmEnrolledMetadataKey: "true"}}, want: true},
		{name: "string 1", auth: &Auth{Metadata: map[string]any{FarmEnrolledMetadataKey: "1"}}, want: true},
		{name: "string false", auth: &Auth{Metadata: map[string]any{FarmEnrolledMetadataKey: "false"}}, want: false},
		{name: "number nonzero", auth: &Auth{Metadata: map[string]any{FarmEnrolledMetadataKey: float64(1)}}, want: true},
		{name: "number zero", auth: &Auth{Metadata: map[string]any{FarmEnrolledMetadataKey: float64(0)}}, want: false},
		{name: "unparseable string", auth: &Auth{Metadata: map[string]any{FarmEnrolledMetadataKey: "enrolled"}}, want: false},
		{name: "wrong type", auth: &Auth{Metadata: map[string]any{FarmEnrolledMetadataKey: []string{"true"}}}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AuthFarmEnrolled(tc.auth); got != tc.want {
				t.Fatalf("AuthFarmEnrolled() = %v, want %v", got, tc.want)
			}
		})
	}
}
