package auth_test

import (
	"testing"

	"github.com/glaicer/gonka-proxy/internal/auth"
)

func TestBearerMatches(t *testing.T) {
	const token = "token-0123456789abcdef"
	tests := []struct {
		name   string
		header string
		token  string
		want   bool
	}{
		{"exact", "Bearer " + token, token, true},
		{"case-insensitive scheme", "bEaReR " + token, token, true},
		{"surrounding spaces", "Bearer   " + token + " ", token, true},
		{"missing header", "", token, false},
		{"wrong token", "Bearer token-0123456789abcdeX", token, false},
		{"prefix of token", "Bearer token-0123", token, false},
		{"token plus suffix", "Bearer " + token + "x", token, false},
		{"other scheme", "Basic " + token, token, false},
		{"no scheme", token, token, false},
		{"empty bearer", "Bearer ", token, false},
		{"empty configured token never matches", "Bearer ", "", false},
		{"empty configured token with value", "Bearer x", "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := auth.BearerMatches(test.header, test.token); got != test.want {
				t.Fatalf("BearerMatches(%q) = %t, want %t", test.header, got, test.want)
			}
		})
	}
}
