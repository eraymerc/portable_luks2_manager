package luks2

import (
	"crypto/pbkdf2"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
	"strconv"

	"golang.org/x/crypto/argon2"
)

var ErrWrongPassphrase = errors.New("parola hatalı (hiçbir anahtar yuvası açılmadı)")

// DataSegment ana anahtarın açacağı veri segmentini ve ona ait digest'i seçer.
func (h *Header) dataSegment() (id string, seg Segment, err error) {
	if len(h.Meta.Config.Requirements.Mandatory) > 0 {
		return "", seg, fmt.Errorf("desteklenmeyen zorunlu gereksinim: %v (yeniden şifreleme yarıda mı kaldı?)",
			h.Meta.Config.Requirements.Mandatory)
	}
	var ids []int
	for k := range h.Meta.Segments {
		n, err := strconv.Atoi(k)
		if err != nil {
			return "", seg, fmt.Errorf("geçersiz segment kimliği %q", k)
		}
		ids = append(ids, n)
	}
	if len(ids) != 1 {
		return "", seg, fmt.Errorf("%d segment var; yalnız tek segmentli diskler destekleniyor", len(ids))
	}
	id = strconv.Itoa(ids[0])
	seg = h.Meta.Segments[id]
	if seg.Type != "crypt" {
		return "", seg, fmt.Errorf("segment türü %q desteklenmiyor", seg.Type)
	}
	if seg.Integrity != nil {
		return "", seg, errors.New("dm-integrity (authenticated encryption) desteklenmiyor")
	}
	return id, seg, nil
}

// Unlock parolayı sırayla anahtar yuvalarında dener, doğru ana anahtarı döndürür.
func (h *Header) Unlock(r io.ReaderAt, passphrase []byte) ([]byte, error) {
	segID, _, err := h.dataSegment()
	if err != nil {
		return nil, err
	}

	// cryptsetup sırası: önce priority=2, sonra normal; priority=0 atlanır.
	type cand struct {
		id   string
		prio int
	}
	var cands []cand
	for id, ks := range h.Meta.Keyslots {
		p := 1
		if ks.Priority != nil {
			p = *ks.Priority
		}
		if p == 0 {
			continue
		}
		cands = append(cands, cand{id, p})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].prio != cands[j].prio {
			return cands[i].prio > cands[j].prio
		}
		a, _ := strconv.Atoi(cands[i].id)
		b, _ := strconv.Atoi(cands[j].id)
		return a < b
	})

	var lastErr error
	for _, c := range cands {
		dg, ok := h.digestFor(c.id, segID)
		if !ok {
			continue // bu yuva veri segmentine ait değil
		}
		mk, err := h.openKeyslot(r, h.Meta.Keyslots[c.id], passphrase)
		if err != nil {
			lastErr = fmt.Errorf("yuva %s: %w", c.id, err)
			continue
		}
		if ok, err := verifyDigest(dg, mk); err != nil {
			lastErr = err
		} else if ok {
			return mk, nil
		}
		clear(mk)
	}
	if lastErr != nil && !errors.Is(lastErr, ErrWrongPassphrase) {
		// Desteklenmeyen KDF vb. gerçek hataları gizleme; ama parola hatası en olası durum.
		return nil, fmt.Errorf("%w; son hata: %v", ErrWrongPassphrase, lastErr)
	}
	return nil, ErrWrongPassphrase
}

func (h *Header) digestFor(keyslot, segment string) (Digest, bool) {
	for _, d := range h.Meta.Digests {
		if contains(d.Keyslots, keyslot) && contains(d.Segments, segment) {
			return d, true
		}
	}
	return Digest{}, false
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func verifyDigest(d Digest, mk []byte) (bool, error) {
	if d.Type != "pbkdf2" {
		return false, fmt.Errorf("digest türü %q desteklenmiyor", d.Type)
	}
	hf, err := hashFunc(d.Hash)
	if err != nil {
		return false, err
	}
	got, err := pbkdf2.Key(hf, string(mk), d.Salt, d.Iterations, len(d.Digest))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, d.Digest) == 1, nil
}

func (h *Header) openKeyslot(r io.ReaderAt, ks Keyslot, pass []byte) ([]byte, error) {
	if ks.Type != "luks2" {
		return nil, fmt.Errorf("yuva türü %q desteklenmiyor", ks.Type)
	}
	if ks.AF.Type != "luks1" && ks.AF.Type != "luksplit" {
		return nil, fmt.Errorf("AF türü %q desteklenmiyor", ks.AF.Type)
	}
	if ks.Area.Type != "raw" {
		return nil, fmt.Errorf("alan türü %q desteklenmiyor", ks.Area.Type)
	}
	if ks.KeySize <= 0 || ks.AF.Stripes <= 0 || ks.KeySize > 512 || ks.AF.Stripes > 1<<16 {
		return nil, errors.New("yuva parametreleri geçersiz")
	}

	areaKey, err := deriveKey(ks.KDF, pass, ks.Area.KeySize)
	if err != nil {
		return nil, err
	}
	defer clear(areaKey)

	off, err := parseU64(ks.Area.Offset, "area.offset")
	if err != nil {
		return nil, err
	}
	size, err := parseU64(ks.Area.Size, "area.size")
	if err != nil {
		return nil, err
	}
	afLen := ks.KeySize * ks.AF.Stripes
	// Anahtar yuvası alanı her zaman 512 baytlık sektörlerle şifrelenir.
	n := (afLen + 511) &^ 511
	if uint64(n) > size {
		return nil, errors.New("AF verisi yuva alanından büyük")
	}
	buf := make([]byte, n)
	defer clear(buf)
	if _, err := r.ReadAt(buf, int64(off)); err != nil {
		return nil, fmt.Errorf("yuva alanı okunamadı: %w", err)
	}
	sc, err := newSectorCipher(ks.Area.Encryption, areaKey)
	if err != nil {
		return nil, err
	}
	for i := 0; i < n/512; i++ {
		sc.decrypt(buf[i*512:(i+1)*512], uint64(i))
	}
	return afMerge(buf[:afLen], ks.KeySize, ks.AF.Stripes, ks.AF.Hash)
}

func deriveKey(k KDF, pass []byte, keyLen int) ([]byte, error) {
	switch k.Type {
	case "pbkdf2":
		hf, err := hashFunc(k.Hash)
		if err != nil {
			return nil, err
		}
		return pbkdf2.Key(hf, string(pass), k.Salt, k.Iterations, keyLen)
	case "argon2i", "argon2id":
		if k.Time <= 0 || k.Memory <= 0 || k.CPUs <= 0 || k.CPUs > 255 {
			return nil, errors.New("argon2 parametreleri geçersiz")
		}
		if k.Type == "argon2i" {
			return argon2.Key(pass, k.Salt, uint32(k.Time), uint32(k.Memory), uint8(k.CPUs), uint32(keyLen)), nil
		}
		return argon2.IDKey(pass, k.Salt, uint32(k.Time), uint32(k.Memory), uint8(k.CPUs), uint32(keyLen)), nil
	}
	return nil, fmt.Errorf("KDF %q desteklenmiyor", k.Type)
}

// afMerge LUKS anti-forensic bölmeyi geri alır.
func afMerge(src []byte, keySize, stripes int, hashName string) ([]byte, error) {
	hf, err := hashFunc(hashName)
	if err != nil {
		return nil, err
	}
	d := make([]byte, keySize)
	for i := 0; i < stripes-1; i++ {
		xorInto(d, src[i*keySize:(i+1)*keySize])
		diffuse(d, hf)
	}
	xorInto(d, src[(stripes-1)*keySize:stripes*keySize])
	return d, nil
}

func xorInto(dst, src []byte) {
	for i := range dst {
		dst[i] ^= src[i]
	}
}

// diffuse: her digest boyutlu blok j, H(be32(j) || blok) ile değiştirilir;
// son kısmi blok kırpılır.
func diffuse(b []byte, hf func() hash.Hash) {
	h := hf()
	ds := h.Size()
	var iv [4]byte
	for j := 0; j*ds < len(b); j++ {
		end := min((j+1)*ds, len(b))
		binary.BigEndian.PutUint32(iv[:], uint32(j))
		h.Reset()
		h.Write(iv[:])
		h.Write(b[j*ds : end])
		copy(b[j*ds:end], h.Sum(nil))
	}
}
