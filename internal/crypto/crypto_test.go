package crypto

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestCipherRoundTrip(t *testing.T) {
	c, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}

	const plaintext = "socks5h://user:secret@proxy.example:1080"
	sealed, nonce, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("secret")) {
		t.Fatal("ciphertext contains plaintext")
	}

	plain, err := c.Decrypt(sealed, nonce)
	if err != nil || plain != plaintext {
		t.Fatalf("plain=%q err=%v", plain, err)
	}

	sealed[0] ^= 1
	if _, err := c.Decrypt(sealed, nonce); err == nil {
		t.Fatal("tampered ciphertext decrypted")
	}
}

func TestCipherUsesFreshNonces(t *testing.T) {
	c, err := New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}

	_, first, err := c.Encrypt("http://proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := c.Encrypt("http://proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("successive encryptions reused a nonce")
	}
}

func TestNewRejectsInvalidKeyLength(t *testing.T) {
	for _, length := range []int{0, 16, 31, 33} {
		t.Run(fmt.Sprintf("length_%d", length), func(t *testing.T) {
			_, err := New(make([]byte, length))
			if !strings.Contains(fmt.Sprint(err), "32 bytes") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestDecryptRejectsInvalidInputs(t *testing.T) {
	c, err := New(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, nonce, err := c.Encrypt("http://proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		ciphertext []byte
		nonce      []byte
		wantErr    string
	}{
		{name: "empty ciphertext", nonce: nonce, wantErr: "ciphertext is empty"},
		{name: "short nonce", ciphertext: sealed, nonce: nonce[:len(nonce)-1], wantErr: "nonce length"},
		{name: "long nonce", ciphertext: sealed, nonce: append(append([]byte(nil), nonce...), 0), wantErr: "nonce length"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.Decrypt(tt.ciphertext, tt.nonce)
			if !strings.Contains(fmt.Sprint(err), tt.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
