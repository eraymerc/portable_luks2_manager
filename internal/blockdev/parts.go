package blockdev

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf16"
)

// Partition bir bölüm tablosu girdisidir.
type Partition struct {
	Number int    // 1'den başlar
	Offset int64  // bayt
	Size   int64  // bayt
	Name   string // GPT bölüm adı
	Scheme string // "gpt" / "mbr"
}

// ReadPartitions GPT'yi (512 ve 4096 baytlık mantıksal sektör) ya da MBR'yi okur.
// Tablo yoksa boş liste döner.
func ReadPartitions(r io.ReaderAt, devSize int64) ([]Partition, error) {
	for _, ss := range []int64{512, 4096} {
		if parts, ok, err := readGPT(r, ss, devSize); err != nil {
			return nil, err
		} else if ok {
			return parts, nil
		}
	}
	return readMBR(r, devSize)
}

func readGPT(r io.ReaderAt, ss, devSize int64) ([]Partition, bool, error) {
	hdr := make([]byte, 512)
	if _, err := r.ReadAt(hdr, ss); err != nil {
		return nil, false, nil
	}
	if string(hdr[:8]) != "EFI PART" {
		return nil, false, nil
	}
	le := binary.LittleEndian
	entLBA := int64(le.Uint64(hdr[72:]))
	n := int64(le.Uint32(hdr[80:]))
	esz := int64(le.Uint32(hdr[84:]))
	if n > 1024 || esz < 128 || esz > 4096 {
		return nil, false, fmt.Errorf("GPT başlığı geçersiz (giriş sayısı %d, boyut %d)", n, esz)
	}
	ents := make([]byte, n*esz)
	if _, err := r.ReadAt(ents, entLBA*ss); err != nil {
		return nil, false, fmt.Errorf("GPT girişleri okunamadı: %w", err)
	}
	var parts []Partition
	for i := int64(0); i < n; i++ {
		e := ents[i*esz : (i+1)*esz]
		if bytes.Equal(e[:16], make([]byte, 16)) {
			continue
		}
		first, last := int64(le.Uint64(e[32:])), int64(le.Uint64(e[40:]))
		if last < first || (last+1)*ss > devSize {
			continue
		}
		u := make([]uint16, 36)
		for k := range u {
			u[k] = le.Uint16(e[56+2*k:])
		}
		name := string(utf16.Decode(u))
		if j := bytes.IndexByte([]byte(name), 0); j >= 0 {
			name = name[:j]
		}
		parts = append(parts, Partition{Number: int(i + 1), Offset: first * ss,
			Size: (last - first + 1) * ss, Name: name, Scheme: "gpt"})
	}
	return parts, true, nil
}

func readMBR(r io.ReaderAt, devSize int64) ([]Partition, error) {
	mbr := make([]byte, 512)
	if _, err := r.ReadAt(mbr, 0); err != nil {
		return nil, err
	}
	if mbr[510] != 0x55 || mbr[511] != 0xaa {
		return nil, nil
	}
	le := binary.LittleEndian
	var parts []Partition
	ext := int64(-1)
	for i := 0; i < 4; i++ {
		e := mbr[446+16*i:]
		typ := e[4]
		start, cnt := int64(le.Uint32(e[8:])), int64(le.Uint32(e[12:]))
		if typ == 0 || cnt == 0 {
			continue
		}
		if typ == 0x05 || typ == 0x0f || typ == 0x85 {
			ext = start
			continue
		}
		if (start+cnt)*512 > devSize {
			continue
		}
		parts = append(parts, Partition{Number: i + 1, Offset: start * 512, Size: cnt * 512, Scheme: "mbr"})
	}
	// Mantıksal bölümler (EBR zinciri), Linux numaralandırması 5'ten başlar.
	num := 5
	for cur := ext; ext >= 0 && cur >= 0 && num < 64; num++ {
		ebr := make([]byte, 512)
		if _, err := r.ReadAt(ebr, cur*512); err != nil || ebr[510] != 0x55 || ebr[511] != 0xaa {
			break
		}
		e1, e2 := ebr[446:], ebr[462:]
		if start, cnt := int64(le.Uint32(e1[8:])), int64(le.Uint32(e1[12:])); e1[4] != 0 && cnt > 0 &&
			(cur+start+cnt)*512 <= devSize {
			parts = append(parts, Partition{Number: num, Offset: (cur + start) * 512, Size: cnt * 512, Scheme: "mbr"})
		}
		if e2[4] == 0 {
			break
		}
		cur = ext + int64(le.Uint32(e2[8:]))
	}
	return parts, nil
}
