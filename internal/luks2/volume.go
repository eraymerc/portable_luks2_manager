package luks2

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"sync"
)

// Device şifreli ham aygıttır (disk bölümü veya imaj dosyası).
type Device interface {
	io.ReaderAt
	io.WriterAt
	Size() int64
	Sync() error
}

// Volume açılmış veri segmentidir. ReadAt/WriteAt düz (çözülmüş) veri üzerinde
// çalışır; ofset ve uzunluk SectorSize'ın katı olmalıdır.
type Volume struct {
	dev        Device
	sc         *sectorCipher
	offset     int64 // segmentin aygıttaki başlangıcı
	size       int64
	sectorSize int
	ivTweak    uint64
}

// OpenVolume ana anahtarla veri segmentini açar.
func (h *Header) OpenVolume(dev Device, masterKey []byte) (*Volume, error) {
	_, seg, err := h.dataSegment()
	if err != nil {
		return nil, err
	}
	ss := seg.SectorSize
	if ss != 512 && ss != 1024 && ss != 2048 && ss != 4096 {
		return nil, fmt.Errorf("geçersiz sektör boyutu %d", ss)
	}
	off, err := parseU64(seg.Offset, "segment.offset")
	if err != nil {
		return nil, err
	}
	var tweak uint64
	if seg.IVTweak != "" {
		if tweak, err = strconv.ParseUint(seg.IVTweak, 10, 64); err != nil {
			return nil, fmt.Errorf("geçersiz iv_tweak %q", seg.IVTweak)
		}
	}
	devSize := dev.Size()
	var size int64
	if seg.Size == "dynamic" {
		size = devSize - int64(off)
	} else {
		s, err := parseU64(seg.Size, "segment.size")
		if err != nil {
			return nil, err
		}
		size = int64(s)
		if int64(off)+size > devSize {
			return nil, errors.New("segment aygıtın sonunu aşıyor")
		}
	}
	size -= size % int64(ss)
	if size <= 0 || off%uint64(ss) != 0 {
		return nil, errors.New("segment boyutu/ofseti geçersiz")
	}
	sc, err := newSectorCipher(seg.Encryption, masterKey)
	if err != nil {
		return nil, err
	}
	return &Volume{dev: dev, sc: sc, offset: int64(off), size: size, sectorSize: ss, ivTweak: tweak}, nil
}

func (v *Volume) Size() int64     { return v.size }
func (v *Volume) SectorSize() int { return v.sectorSize }
func (v *Volume) Sync() error     { return v.dev.Sync() }

// ivFor düz veri ofsetindeki sektörün IV numarası. LUKS2 sektör boyutu ne
// olursa olsun IV'yi 512 baytlık birimde sayar (dm-crypt iv_large_sectors
// KULLANILMAZ): 4096 baytlık sektörlerde IV 0, 8, 16, ... diye ilerler.
// cryptsetup ile doğrulandı (luks2_test.go). iv_tweak da 512 baytlık birimdedir.
func (v *Volume) ivFor(off int64) uint64 {
	return uint64(off)/512 + v.ivTweak
}

func (v *Volume) check(off int64, n int) error {
	if off%int64(v.sectorSize) != 0 || n%v.sectorSize != 0 {
		return fmt.Errorf("hizasız erişim: ofset %d uzunluk %d", off, n)
	}
	if off < 0 || off+int64(n) > v.size {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func (v *Volume) ReadAt(p []byte, off int64) (int, error) {
	if err := v.check(off, len(p)); err != nil {
		return 0, err
	}
	if _, err := v.dev.ReadAt(p, v.offset+off); err != nil {
		return 0, err
	}
	v.crypt(p, off, v.sc.decrypt)
	return len(p), nil
}

// crypt sektörleri işler; büyük tamponlarda işi çekirdeklere böler
// (x/crypto/xts tek çekirdekte ~150-300 MB/s, USB disk hızının altında kalabiliyor).
func (v *Volume) crypt(p []byte, off int64, fn func([]byte, uint64)) {
	ss := v.sectorSize
	sectors := len(p) / ss
	workers := min(runtime.NumCPU(), sectors/(parallelMin/ss))
	if workers < 2 {
		for i := 0; i < len(p); i += ss {
			fn(p[i:i+ss], v.ivFor(off+int64(i)))
		}
		return
	}
	per := (sectors + workers - 1) / workers
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo, hi := w*per*ss, min((w+1)*per*ss, len(p))
		if lo >= hi {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := lo; i < hi; i += ss {
				fn(p[i:i+ss], v.ivFor(off+int64(i)))
			}
		}()
	}
	wg.Wait()
}

// parallelMin bu boyuttan küçük parçalar için ayrı goroutine açılmaz.
const parallelMin = 128 << 10

// WriteAt p'yi bozmadan şifreleyip yazar.
func (v *Volume) WriteAt(p []byte, off int64) (int, error) {
	if err := v.check(off, len(p)); err != nil {
		return 0, err
	}
	buf := make([]byte, len(p))
	copy(buf, p)
	v.crypt(buf, off, v.sc.encrypt)
	if _, err := v.dev.WriteAt(buf, v.offset+off); err != nil {
		return 0, err
	}
	return len(p), nil
}
