package ext2fs

import (
	"container/list"
	"io"
	"math/bits"
	"slices"
)

// SectorDevice sektör hizalı erişim isteyen alt aygıt (luks2.Volume).
type SectorDevice interface {
	io.ReaderAt
	io.WriterAt
	Size() int64
	SectorSize() int
	Sync() error
}

const (
	chunkSize   = 256 << 10
	cacheChunks = 128      // 32 MiB
	maxDirty    = 24 << 20 // bu kadar kirli veri birikince diske yaz
	maxRun      = 4 << 20  // tek yazma çağrısının üst sınırı
)

// chunkCache libext2fs'in küçük (blok boyutlu) isteklerini büyük hizalı G/Ç'ye
// çeviren, çözülmüş veriyi tutan geri-yazmalı (write-back) önbellek.
//
//   - Okuma: 256 KiB'lik parçalar halinde okunur, LRU'da tutulur.
//   - Yazma: veri önbellekteki parçaya yazılır ve sektör sektör "kirli" işaretlenir;
//     diske hemen yazılmaz. libext2fs her veri bloğundan sonra inode tablosunu da
//     yazdığından bu, aynı bloğa binlerce yazmayı teke indirir.
//   - Boşaltma (Sync = libext2fs io_channel_flush, kirli veri sınırı, kirli parçanın
//     LRU'dan atılması): kirli bölgeler ofset sırasıyla birleştirilip büyük
//     parçalar halinde yazılır.
//
// Hiç okunmamış bir parçaya yazarken parça diskten okunmaz; yalnızca yazılan
// sektörler "geçerli" sayılır, okuma gerekince eksik sektörler doldurulur.
//
// Eşzamanlı erişim FS kilidi altında olduğundan kendi kilidi yoktur.
type chunkCache struct {
	dev     SectorDevice
	ss      int64
	size    int64
	spc     int        // parça başına sektör
	lru     *list.List // *chunk, önde en yeni
	index   map[int64]*list.Element
	dirty   int64 // kirli bayt
	scratch []byte
}

type chunk struct {
	n     int64
	data  []byte
	valid bitset // sektör içeriği bellekte doğru
	dirty bitset // sektör diske yazılmayı bekliyor
}

type bitset []uint64

func newBitset(n int) bitset    { return make(bitset, (n+63)/64) }
func (b bitset) set(i int)      { b[i/64] |= 1 << (i % 64) }
func (b bitset) has(i int) bool { return b[i/64]&(1<<(i%64)) != 0 }
func (b bitset) reset()         { clear(b) }
func (b bitset) count() int {
	c := 0
	for _, w := range b {
		c += bits.OnesCount64(w)
	}
	return c
}

func newChunkCache(dev SectorDevice) *chunkCache {
	ss := int64(dev.SectorSize())
	return &chunkCache{dev: dev, ss: ss, size: dev.Size(), spc: int(chunkSize / ss),
		lru: list.New(), index: map[int64]*list.Element{}}
}

func (c *chunkCache) chunkLen(n int64) int64 { return min(int64(chunkSize), c.size-n*chunkSize) }

// slot n numaralı parçayı döndürür; yoksa (gerekirse LRU'dan birini atıp) boş açar.
func (c *chunkCache) slot(n int64) (*chunk, error) {
	if e, ok := c.index[n]; ok {
		c.lru.MoveToFront(e)
		return e.Value.(*chunk), nil
	}
	var ch *chunk
	if c.lru.Len() >= cacheChunks {
		e := c.lru.Back()
		old := e.Value.(*chunk)
		if old.dirty.count() > 0 {
			// Kirli parçayı atmadan önce tüm kirli veriyi sırayla yaz
			// (tek parçayı yazmaktan daha iyi birleşir).
			if err := c.flushDirty(); err != nil {
				return nil, err
			}
		}
		c.lru.Remove(e)
		delete(c.index, old.n)
		ch = old
		ch.valid.reset()
		ch.dirty.reset()
	} else {
		ch = &chunk{data: make([]byte, chunkSize), valid: newBitset(c.spc), dirty: newBitset(c.spc)}
	}
	ch.n = n
	ch.data = ch.data[:c.chunkLen(n)]
	c.index[n] = c.lru.PushFront(ch)
	return ch, nil
}

// fill parçanın geçersiz sektörlerini diskten doldurur.
func (c *chunkCache) fill(ch *chunk) error {
	ns := int(int64(len(ch.data)) / c.ss)
	nv := ch.valid.count()
	if nv >= ns {
		return nil
	}
	if nv == 0 {
		if _, err := c.dev.ReadAt(ch.data, ch.n*chunkSize); err != nil {
			return err
		}
	} else {
		if c.scratch == nil {
			c.scratch = make([]byte, chunkSize)
		}
		tmp := c.scratch[:len(ch.data)]
		if _, err := c.dev.ReadAt(tmp, ch.n*chunkSize); err != nil {
			return err
		}
		for i := 0; i < ns; i++ {
			if !ch.valid.has(i) {
				o := int64(i) * c.ss
				copy(ch.data[o:o+c.ss], tmp[o:o+c.ss])
			}
		}
	}
	for i := 0; i < ns; i++ {
		ch.valid.set(i)
	}
	return nil
}

func (c *chunkCache) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > c.size {
		return 0, io.ErrUnexpectedEOF
	}
	done := 0
	for done < len(p) {
		pos := off + int64(done)
		ch, err := c.slot(pos / chunkSize)
		if err != nil {
			return done, err
		}
		if err := c.fill(ch); err != nil {
			return done, err
		}
		done += copy(p[done:], ch.data[pos%chunkSize:])
	}
	return done, nil
}

func (c *chunkCache) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > c.size {
		return 0, io.ErrUnexpectedEOF
	}
	// Sektör sınırına genişlet; kısmi sektörlerin eski içeriği önce okunur.
	start := off - off%c.ss
	end := off + int64(len(p))
	if r := end % c.ss; r != 0 {
		end += c.ss - r
	}
	buf := p
	if start != off || end != off+int64(len(p)) {
		buf = make([]byte, end-start)
		if _, err := c.ReadAt(buf[:c.ss], start); err != nil {
			return 0, err
		}
		if _, err := c.ReadAt(buf[len(buf)-int(c.ss):], end-c.ss); err != nil {
			return 0, err
		}
		copy(buf[off-start:], p)
	}
	for pos := start; pos < end; {
		n := pos / chunkSize
		cend := min((n+1)*chunkSize, end)
		ch, err := c.slot(n)
		if err != nil {
			return 0, err
		}
		copy(ch.data[pos%chunkSize:], buf[pos-start:cend-start])
		for s := int((pos - n*chunkSize) / c.ss); s < int((cend-n*chunkSize)/c.ss); s++ {
			ch.valid.set(s)
			if !ch.dirty.has(s) {
				ch.dirty.set(s)
				c.dirty += c.ss
			}
		}
		pos = cend
	}
	if c.dirty >= maxDirty {
		if err := c.flushDirty(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// flushDirty tüm kirli sektörleri ofset sırasıyla, bitişik olanları birleştirerek yazar.
func (c *chunkCache) flushDirty() error {
	if c.dirty == 0 {
		return nil
	}
	var chunks []*chunk
	for _, e := range c.index {
		if ch := e.Value.(*chunk); ch.dirty.count() > 0 {
			chunks = append(chunks, ch)
		}
	}
	slices.SortFunc(chunks, func(a, b *chunk) int {
		switch {
		case a.n < b.n:
			return -1
		case a.n > b.n:
			return 1
		}
		return 0
	})

	run := make([]byte, 0, maxRun)
	runOff := int64(0)
	write := func() error {
		if len(run) == 0 {
			return nil
		}
		_, err := c.dev.WriteAt(run, runOff)
		run = run[:0]
		return err
	}
	var werr error
	for _, ch := range chunks {
		ns := int(int64(len(ch.data)) / c.ss)
		for s := 0; s < ns && werr == nil; s++ {
			if !ch.dirty.has(s) {
				continue
			}
			abs := ch.n*chunkSize + int64(s)*c.ss
			if len(run) > 0 && (abs != runOff+int64(len(run)) || len(run)+int(c.ss) > maxRun) {
				werr = write()
			}
			if len(run) == 0 {
				runOff = abs
			}
			o := int64(s) * c.ss
			run = append(run, ch.data[o:o+c.ss]...)
		}
		ch.dirty.reset()
	}
	if werr == nil {
		werr = write()
	}
	c.dirty = 0
	if werr != nil {
		// Yazılamayan veri kayboldu; diskle tutarsız önbelleği at.
		c.invalidate()
	}
	return werr
}

func (c *chunkCache) Sync() error {
	if err := c.flushDirty(); err != nil {
		return err
	}
	return c.dev.Sync()
}

// invalidate tüm önbelleği (kirli veri dahil) atar.
func (c *chunkCache) invalidate() {
	c.lru.Init()
	clear(c.index)
	c.dirty = 0
}
