package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

type Decrypter struct {
	aead cipher.AEAD
}

func NewDecrypter(secret string) (*Decrypter, error) {
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Decrypter{aead: aead}, nil
}

// Decrypt accepts the Node implementation's ivHex:tagHex:cipherHex format.
func (d *Decrypter) Decrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid encrypted value")
	}
	iv, err := hex.DecodeString(parts[0])
	if err != nil || len(iv) != d.aead.NonceSize() {
		return "", fmt.Errorf("invalid encrypted iv")
	}
	tag, err := hex.DecodeString(parts[1])
	if err != nil || len(tag) != d.aead.Overhead() {
		return "", fmt.Errorf("invalid encrypted tag")
	}
	content, err := hex.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("invalid encrypted content")
	}
	sealed := append(append(make([]byte, 0, len(content)+len(tag)), content...), tag...)
	plain, err := d.aead.Open(nil, iv, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt value: %w", err)
	}
	return string(plain), nil
}

// Encrypt produces the exact ivHex:tagHex:cipherHex format used by Node.
func (d *Decrypter) Encrypt(value string) (string, error) {
	iv := make([]byte, d.aead.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", fmt.Errorf("generate encryption nonce: %w", err)
	}
	sealed := d.aead.Seal(nil, iv, []byte(value), nil)
	content, tag := sealed[:len(sealed)-d.aead.Overhead()], sealed[len(sealed)-d.aead.Overhead():]
	return hex.EncodeToString(iv) + ":" + hex.EncodeToString(tag) + ":" + hex.EncodeToString(content), nil
}
