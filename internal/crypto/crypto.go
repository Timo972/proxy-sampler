// Package crypto encrypts and decrypts stored proxy credentials.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Cipher protects proxy URLs using AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// New constructs a Cipher from a 32-byte AES-256 key.
func New(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, errors.New("encryption key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM cipher: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt seals plaintext using a fresh random nonce.
func (c *Cipher) Encrypt(plaintext string) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate encryption nonce: %w", err)
	}
	ciphertext = c.aead.Seal(nil, nonce, []byte(plaintext), nil)
	return ciphertext, nonce, nil
}

// Decrypt authenticates and opens ciphertext.
func (c *Cipher) Decrypt(ciphertext, nonce []byte) (string, error) {
	if len(ciphertext) == 0 {
		return "", errors.New("decrypt proxy credentials: ciphertext is empty")
	}
	if len(nonce) != c.aead.NonceSize() {
		return "", fmt.Errorf("decrypt proxy credentials: nonce length is %d, want %d", len(nonce), c.aead.NonceSize())
	}
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt proxy credentials: %w", err)
	}
	return string(plaintext), nil
}
