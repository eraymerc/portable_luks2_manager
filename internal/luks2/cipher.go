package luks2

import (
	"crypto/aes"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"hash"
	"strings"

	"golang.org/x/crypto/xts"
)

// sectorCipher tek bir sektörü yerinde şifreler/çözer. iv: IV sektör numarası.
type sectorCipher struct {
	c       *xts.Cipher
	plain32 bool // "plain": IV 32 bite kırpılır
}

// newSectorCipher "aes-xts-plain64" biçimindeki şifre tanımını açar.
func newSectorCipher(spec string, key []byte) (*sectorCipher, error) {
	parts := strings.SplitN(spec, "-", 3)
	if len(parts) != 3 || parts[0] != "aes" || parts[1] != "xts" {
		return nil, fmt.Errorf("desteklenmeyen şifre %q (yalnız aes-xts-plain64)", spec)
	}
	sc := &sectorCipher{}
	switch parts[2] {
	case "plain64":
	case "plain":
		sc.plain32 = true
	default:
		return nil, fmt.Errorf("desteklenmeyen IV üreteci %q", parts[2])
	}
	if len(key) != 32 && len(key) != 64 {
		return nil, fmt.Errorf("aes-xts için geçersiz anahtar uzunluğu %d", len(key))
	}
	c, err := xts.NewCipher(aes.NewCipher, key)
	if err != nil {
		return nil, err
	}
	sc.c = c
	return sc, nil
}

func (s *sectorCipher) iv(n uint64) uint64 {
	if s.plain32 {
		return n & 0xffffffff
	}
	return n
}

func (s *sectorCipher) decrypt(buf []byte, iv uint64) { s.c.Decrypt(buf, buf, s.iv(iv)) }
func (s *sectorCipher) encrypt(buf []byte, iv uint64) { s.c.Encrypt(buf, buf, s.iv(iv)) }

func hashFunc(name string) (func() hash.Hash, error) {
	switch strings.ToLower(name) {
	case "sha1":
		return sha1.New, nil
	case "sha224":
		return sha256.New224, nil
	case "sha256":
		return sha256.New, nil
	case "sha384":
		return sha512.New384, nil
	case "sha512":
		return sha512.New, nil
	}
	return nil, fmt.Errorf("desteklenmeyen hash %q", name)
}
