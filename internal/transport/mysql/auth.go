package mysql

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// nativeScramble implements mysql_native_password:
// SHA1(password) XOR SHA1(salt + SHA1(SHA1(password))).
func nativeScramble(password string, salt []byte) []byte {
	if password == "" {
		return nil
	}
	stage1 := sha1.Sum([]byte(password))
	stage2 := sha1.Sum(stage1[:])
	h := sha1.New()
	h.Write(salt)
	h.Write(stage2[:])
	out := h.Sum(nil)
	for i := range out {
		out[i] ^= stage1[i]
	}
	return out
}

// sha2Scramble implements the fast path of caching_sha2_password:
// SHA256(password) XOR SHA256(SHA256(SHA256(password)) + salt).
func sha2Scramble(password string, salt []byte) []byte {
	if password == "" {
		return nil
	}
	stage1 := sha256.Sum256([]byte(password))
	stage2 := sha256.Sum256(stage1[:])
	h := sha256.New()
	h.Write(stage2[:])
	h.Write(salt)
	out := h.Sum(nil)
	for i := range out {
		out[i] ^= stage1[i]
	}
	return out
}

// encryptPassword protects a password for transfer over a plain connection
// with the server's RSA public key, as sha256_password and the full
// authentication of caching_sha2_password require.
func encryptPassword(password string, salt, pemKey []byte) ([]byte, error) {
	block, _ := pem.Decode(pemKey)
	if block == nil {
		return nil, errors.New("server sent an invalid public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse server public key: %w", err)
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("server public key is not RSA")
	}
	plain := append([]byte(password), 0)
	for i := range plain {
		plain[i] ^= salt[i%len(salt)]
	}
	return rsa.EncryptOAEP(sha1.New(), rand.Reader, key, plain, nil)
}
