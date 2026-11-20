package settings

import (
	"slices"
	"testing"
)

func TestEmailAllowed(t *testing.T) {
	a := Auth{AllowedEmailDomains: []string{"example.com", "corp.example.org"}}
	tests := map[string]bool{
		"alice@example.com":      true,
		"Alice@EXAMPLE.com":      true,
		"bob@corp.example.org":   true,
		"eve@evil.com":           false,
		"eve@example.com.evil":   false,
		"eve@sub.example.com":    false,
		"no-at-sign":             false,
		"@example.com":           false,
		"trailing@":              false,
		"eve@evil.com@example.c": false,
	}
	for email, want := range tests {
		if got := a.EmailAllowed(email); got != want {
			t.Errorf("EmailAllowed(%q) = %v, want %v", email, got, want)
		}
	}
	if (Auth{}).EmailAllowed("alice@example.com") {
		t.Error("with no domains configured, nothing is allowed")
	}
}

func TestParseDomains(t *testing.T) {
	got := ParseDomains(" example.com, @Corp.Example.org\nother.io  ,, ")
	if want := []string{"example.com", "corp.example.org", "other.io"}; !slices.Equal(got, want) {
		t.Errorf("ParseDomains = %v, want %v", got, want)
	}
}
