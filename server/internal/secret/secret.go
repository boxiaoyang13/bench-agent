// Package secret 用 AES-GCM 加解密 Target 的 api_key。
//
// 原则：明文只存在于内存；任何响应、日志、快照都不包含明文（见 docs/API.md §1.3）。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
)

type Box struct {
	aead cipher.AEAD
}

// New 用 32 字节主密钥构造。
func New(masterKey []byte) (*Box, error) {
	if len(masterKey) != 32 {
		return nil, errors.New("主密钥必须为 32 字节")
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Encrypt 返回 nonce||ciphertext。
func (b *Box) Encrypt(plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, []byte(plain), nil), nil
}

// Decrypt 解开 nonce||ciphertext。
func (b *Box) Decrypt(blob []byte) (string, error) {
	if len(blob) == 0 {
		return "", nil
	}
	ns := b.aead.NonceSize()
	if len(blob) < ns {
		return "", errors.New("密文长度非法")
	}
	plain, err := b.aead.Open(nil, blob[:ns], blob[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// Hint 生成展示用的掩码（保留后 4 位）。
func Hint(plain string) string {
	if plain == "" {
		return ""
	}
	if len(plain) <= 4 {
		return "••••"
	}
	return "••••••••" + plain[len(plain)-4:]
}

// Fingerprint 返回主密钥指纹（前 4 字节 base64），用于启动日志确认密钥正确。
func Fingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return base64.RawStdEncoding.EncodeToString(sum[:3])
}
