// Package luks2 LUKS2 başlığını okur, parolayla ana anahtarı çözer ve veri
// segmentini sektör sektör şifreleyip çözen bir cihaz sunar.
//
// Referans: cryptsetup "LUKS2 On-Disk Format Specification" (docs/on-disk-format-luks2.pdf).
package luks2

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"
)

const binHeaderSize = 4096

var (
	magic1 = []byte{'L', 'U', 'K', 'S', 0xba, 0xbe}
	magic2 = []byte{'S', 'K', 'U', 'L', 0xba, 0xbe}

	// Birincil başlık bozuksa ikincilin aranacağı ofsetler (cryptsetup ile aynı).
	secondaryOffsets = []int64{0x4000, 0x8000, 0x10000, 0x20000, 0x40000,
		0x80000, 0x100000, 0x200000, 0x400000}

	ErrNotLUKS2 = errors.New("LUKS2 başlığı bulunamadı")
)

// binaryHeader diskteki 4096 baytlık ikili başlığın ilk 512 baytıdır.
type binaryHeader struct {
	Magic     [6]byte
	Version   uint16
	HdrSize   uint64
	SeqID     uint64
	Label     [48]byte
	CsumAlg   [32]byte
	Salt      [64]byte
	UUID      [40]byte
	Subsystem [48]byte
	HdrOffset uint64
	_         [184]byte
	Csum      [64]byte
}

// Header çözümlenmiş ve sağlama toplamı doğrulanmış LUKS2 başlığıdır.
type Header struct {
	SeqID   uint64
	HdrSize uint64
	Label   string
	UUID    string
	Meta    Metadata
}

// Metadata JSON alanıdır (yalnız kullandığımız kısımlar).
type Metadata struct {
	Keyslots map[string]Keyslot `json:"keyslots"`
	Segments map[string]Segment `json:"segments"`
	Digests  map[string]Digest  `json:"digests"`
	Config   struct {
		JSONSize     string   `json:"json_size"`
		KeyslotsSize string   `json:"keyslots_size"`
		Flags        []string `json:"flags"`
		Requirements struct {
			Mandatory []string `json:"mandatory"`
		} `json:"requirements"`
	} `json:"config"`
}

type Keyslot struct {
	Type     string `json:"type"`
	KeySize  int    `json:"key_size"`
	Priority *int   `json:"priority"`
	AF       struct {
		Type    string `json:"type"`
		Stripes int    `json:"stripes"`
		Hash    string `json:"hash"`
	} `json:"af"`
	Area struct {
		Type       string `json:"type"`
		Offset     string `json:"offset"`
		Size       string `json:"size"`
		Encryption string `json:"encryption"`
		KeySize    int    `json:"key_size"`
	} `json:"area"`
	KDF KDF `json:"kdf"`
}

type KDF struct {
	Type       string `json:"type"`
	Salt       []byte `json:"salt"` // base64 -> []byte (encoding/json yapar)
	Hash       string `json:"hash"`
	Iterations int    `json:"iterations"`
	Time       int    `json:"time"`
	Memory     int    `json:"memory"` // KiB
	CPUs       int    `json:"cpus"`
}

type Segment struct {
	Type       string   `json:"type"`
	Offset     string   `json:"offset"`
	Size       string   `json:"size"` // bayt veya "dynamic"
	IVTweak    string   `json:"iv_tweak"`
	Encryption string   `json:"encryption"`
	SectorSize int      `json:"sector_size"`
	Integrity  any      `json:"integrity"`
	Flags      []string `json:"flags"`
}

type Digest struct {
	Type       string   `json:"type"`
	Keyslots   []string `json:"keyslots"`
	Segments   []string `json:"segments"`
	Salt       []byte   `json:"salt"`
	Digest     []byte   `json:"digest"`
	Hash       string   `json:"hash"`
	Iterations int      `json:"iterations"`
}

// HasMagic r'nin başında LUKS imzası olup olmadığını söyler (sürüm fark etmez).
func HasMagic(r io.ReaderAt) bool {
	var b [6]byte
	if _, err := r.ReadAt(b[:], 0); err != nil {
		return false
	}
	return bytes.Equal(b[:], magic1)
}

// ReadHeader birincil ve ikincil başlıkları okur, geçerli olanlardan seqid'i
// büyük olanı döndürür.
func ReadHeader(r io.ReaderAt) (*Header, error) {
	primary, perr := readHeaderAt(r, 0, magic1)
	var offsets []int64
	if primary != nil {
		offsets = []int64{int64(primary.HdrSize)}
	} else {
		offsets = secondaryOffsets
	}
	var secondary *Header
	for _, off := range offsets {
		if h, err := readHeaderAt(r, off, magic2); err == nil {
			secondary = h
			break
		}
	}
	switch {
	case primary == nil && secondary == nil:
		if perr != nil {
			return nil, perr
		}
		return nil, ErrNotLUKS2
	case primary == nil:
		return secondary, nil
	case secondary == nil:
		return primary, nil
	case secondary.SeqID > primary.SeqID:
		return secondary, nil
	default:
		return primary, nil
	}
}

func readHeaderAt(r io.ReaderAt, off int64, wantMagic []byte) (*Header, error) {
	bin := make([]byte, binHeaderSize)
	if _, err := r.ReadAt(bin, off); err != nil {
		return nil, fmt.Errorf("başlık okunamadı: %w", err)
	}
	var bh binaryHeader
	if err := binary.Read(bytes.NewReader(bin), binary.BigEndian, &bh); err != nil {
		return nil, err
	}
	if !bytes.Equal(bh.Magic[:], wantMagic) {
		return nil, ErrNotLUKS2
	}
	if bh.Version != 2 {
		return nil, fmt.Errorf("LUKS sürüm %d desteklenmiyor (yalnız LUKS2)", bh.Version)
	}
	if uint64(off) != bh.HdrOffset {
		return nil, fmt.Errorf("başlık ofseti tutarsız (%d != %d)", off, bh.HdrOffset)
	}
	// Geçerli boyutlar: 16 KiB .. 4 MiB, ikinin kuvveti
	if bh.HdrSize < 0x4000 || bh.HdrSize > 0x400000 || bh.HdrSize&(bh.HdrSize-1) != 0 {
		return nil, fmt.Errorf("geçersiz başlık boyutu %d", bh.HdrSize)
	}

	full := make([]byte, bh.HdrSize)
	copy(full, bin)
	if _, err := r.ReadAt(full[binHeaderSize:], off+binHeaderSize); err != nil {
		return nil, fmt.Errorf("JSON alanı okunamadı: %w", err)
	}

	alg := cstr(bh.CsumAlg[:])
	var h hash.Hash
	switch alg {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		return nil, fmt.Errorf("desteklenmeyen başlık sağlama algoritması %q", alg)
	}
	// Sağlama alanı (0x1C0, 64 bayt) sıfırlanarak hesaplanır.
	const csumOff = 0x1c0
	csum := append([]byte(nil), full[csumOff:csumOff+64]...)
	clear(full[csumOff : csumOff+64])
	h.Write(full)
	sum := h.Sum(nil)
	if !bytes.Equal(sum, csum[:len(sum)]) {
		return nil, errors.New("başlık sağlama toplamı hatalı")
	}

	js := full[binHeaderSize:]
	if i := bytes.IndexByte(js, 0); i >= 0 {
		js = js[:i]
	}
	var meta Metadata
	if err := json.Unmarshal(js, &meta); err != nil {
		return nil, fmt.Errorf("JSON metadata çözülemedi: %w", err)
	}
	return &Header{
		SeqID:   bh.SeqID,
		HdrSize: bh.HdrSize,
		Label:   cstr(bh.Label[:]),
		UUID:    cstr(bh.UUID[:]),
		Meta:    meta,
	}, nil
}

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func parseU64(s, what string) (uint64, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: geçersiz sayı %q", what, s)
	}
	return v, nil
}
