package secretbox

import (
	"bytes"
	"errors"
	"testing"
)

func TestSealOpen(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	box, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed := box.Seal([]byte("sk-secret"))
	if bytes.Contains(sealed, []byte("sk-secret")) {
		t.Fatal("plaintext visible in sealed data")
	}
	if again := box.Seal([]byte("sk-secret")); bytes.Equal(again, sealed) {
		t.Error("two seals of the same value are identical; the nonce isn't random")
	}
	got, err := box.Open(sealed)
	if err != nil || string(got) != "sk-secret" {
		t.Fatalf("Open = %q, %v", got, err)
	}

	other, _ := New(bytes.Repeat([]byte{2}, 32))
	if _, err := other.Open(sealed); !errors.Is(err, ErrDecrypt) {
		t.Errorf("open with another key: err = %v, want ErrDecrypt", err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := box.Open(sealed); !errors.Is(err, ErrDecrypt) {
		t.Errorf("open tampered data: err = %v, want ErrDecrypt", err)
	}
	if _, err := New([]byte("short")); err == nil {
		t.Error("short key accepted")
	}
}
