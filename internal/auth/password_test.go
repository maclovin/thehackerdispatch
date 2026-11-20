package auth

import (
	"strings"
	"testing"
)

func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		pw, err := GeneratePassword()
		if err != nil {
			t.Fatalf("GeneratePassword: %v", err)
		}
		if len(pw) != passwordLength {
			t.Fatalf("len = %d, want %d", len(pw), passwordLength)
		}
		for _, c := range pw {
			if !strings.ContainsRune(passwordAlphabet, c) {
				t.Fatalf("password %q has character %q outside the alphabet", pw, c)
			}
		}
		if seen[pw] {
			t.Fatalf("duplicate password %q", pw)
		}
		seen[pw] = true
	}
}

func TestHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct horse")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("unexpected hash format: %s", hash)
	}

	tests := []struct {
		password string
		want     bool
	}{
		{"correct horse", true},
		{"correct horse ", false},
		{"", false},
	}
	for _, tt := range tests {
		got, err := VerifyPassword(tt.password, hash)
		if err != nil {
			t.Fatalf("VerifyPassword(%q): %v", tt.password, err)
		}
		if got != tt.want {
			t.Errorf("VerifyPassword(%q) = %v, want %v", tt.password, got, tt.want)
		}
	}
}

func TestVerifyMalformedHash(t *testing.T) {
	for _, hash := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$AA$AA", "$argon2id$v=19$m=x$AA$AA"} {
		if _, err := VerifyPassword("x", hash); err == nil {
			t.Errorf("VerifyPassword with hash %q: want error", hash)
		}
	}
}
