// Package ext2fs libext2fs (e2fsprogs) üzerinden ext2/3/4 dosya sistemine
// erişir. ext4 mantığı e2fsprogs'undur; bu paket yalnızca G/Ç'yi şifreli
// aygıta yönlendirir ve güvenli bir Go API'si sunar.
//
// Tüm libext2fs çağrıları tek bir kilit altında yapılır (kütüphane iş
// parçacığı güvenli değildir).
package ext2fs

/*
#include "lw.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"path"
	"runtime/cgo"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	ErrNotFound = errors.New("bulunamadı")
	ErrExists   = errors.New("zaten var")
	ErrReadOnly = errors.New("dosya sistemi salt-okunur")
	ErrNotEmpty = errors.New("dizin boş değil")
	ErrNoSpace  = errors.New("diskte yer yok")
	ErrNotDir   = errors.New("dizin değil")
	ErrIsDir    = errors.New("bir dizin")
	ErrBadName  = errors.New("geçersiz ad")
	ErrBusy     = errors.New("dosya şu an kullanımda")
	ErrClosed   = errors.New("dosya sistemi kapalı")
)

func init() {
	C.initialize_ext2_error_table()
}

// Error libext2fs hata kodunu taşır.
type Error struct {
	Op   string
	Code int64
	Msg  string
	IO   error // G/Ç katmanından gelen asıl hata
}

func (e *Error) Error() string {
	s := e.Op + ": " + e.Msg
	if e.IO != nil {
		s += " (" + e.IO.Error() + ")"
	}
	return s
}

func (e *Error) Unwrap() error {
	switch e.Code {
	case int64(C.EXT2_ET_FILE_NOT_FOUND):
		return ErrNotFound
	case int64(C.EXT2_ET_FILE_EXISTS), int64(C.EXT2_ET_DIR_EXISTS):
		return ErrExists
	case int64(C.EXT2_ET_RO_FILSYS):
		return ErrReadOnly
	case int64(C.EXT2_ET_BLOCK_ALLOC_FAIL), int64(C.EXT2_ET_INODE_ALLOC_FAIL), int64(syscall.ENOSPC):
		return ErrNoSpace
	case int64(C.EXT2_ET_NO_DIRECTORY):
		return ErrNotDir
	}
	return e.IO
}

// Entry bir dizin girdisi.
type Entry struct {
	Name   string    `json:"name"`
	Ino    uint32    `json:"ino"`
	Type   string    `json:"type"` // file, dir, symlink, other
	Mode   uint32    `json:"mode"`
	Size   uint64    `json:"size"`
	Mtime  time.Time `json:"mtime"`
	UID    uint32    `json:"uid"`
	GID    uint32    `json:"gid"`
	Target string    `json:"target,omitempty"` // sembolik bağ hedefi
}

// Info dosya sistemi özeti.
type Info struct {
	Label      string `json:"label"`
	UUID       string `json:"uuid"`
	BlockSize  uint32 `json:"blockSize"`
	TotalBytes uint64 `json:"totalBytes"`
	FreeBytes  uint64 `json:"freeBytes"` // kök için ayrılmış hariç
	Inodes     uint32 `json:"inodes"`
	FreeInodes uint32 `json:"freeInodes"`
	ReadOnly   bool   `json:"readOnly"`
	ROReason   string `json:"roReason,omitempty"`
	Features   string `json:"features"`
}

// FS açık bir dosya sistemi.
type FS struct {
	mu       sync.Mutex
	fs       C.ext2_filsys
	ioh      cgo.Handle
	io       *ioState
	readOnly bool
	roReason string
	marked   bool // süper blok diskte "temiz değil" olarak işaretli
	bitmaps  bool
	busy     map[uint32]int // açık (okunan/yazılan) inode'lar
	writers  int            // süren yazma işlemleri
	closing  bool
	cond     *sync.Cond // writers azalınca haber verir
}

const (
	fsStateValid = 0x0001 // EXT2_VALID_FS
	fsStateError = 0x0002 // EXT2_ERROR_FS
)

// Open aygıttaki dosya sistemini açar. writable=false ise salt-okunur açılır;
// true ise dosya sistemi uygun değilse yine salt-okunur açılıp sebebi
// Info().ROReason'da verilir.
func Open(dev SectorDevice, writable bool) (*FS, error) {
	st := &ioState{cache: newChunkCache(dev)}
	h := cgo.NewHandle(st)
	f := &FS{ioh: h, io: st, busy: map[uint32]int{}}
	f.cond = sync.NewCond(&f.mu)

	name := C.CString(fmt.Sprintf("lw:%d", uintptr(h)))
	defer C.free(unsafe.Pointer(name))

	open := func(rw bool) C.errcode_t {
		flags := C.int(C.EXT2_FLAG_64BITS)
		if rw {
			flags |= C.EXT2_FLAG_RW
		}
		st.readOnly = !rw
		return C.ext2fs_open2(name, nil, flags, 0, 0, C.lw_io_manager, &f.fs)
	}

	rw := writable
	code := open(rw)
	if code == C.EXT2_ET_RO_UNSUPP_FEATURE && rw {
		rw = false
		f.roReason = "dosya sisteminde bu aracın yazamadığı bir özellik var (ro_compat)"
		code = open(false)
	}
	if code != 0 {
		h.Delete()
		return nil, f.err("dosya sistemi açılamadı", code)
	}

	if rw {
		if reason := f.writeBlocker(); reason != "" {
			f.roReason = reason
			C.ext2fs_close_free(&f.fs)
			if code := open(false); code != 0 {
				h.Delete()
				return nil, f.err("dosya sistemi açılamadı", code)
			}
			rw = false
		}
	}
	if !writable && f.roReason == "" {
		f.roReason = "salt-okunur açıldı"
	}
	f.readOnly = !rw
	if rw {
		// Yedek süper blok/grup tanımlayıcılarına dokunma (çekirdek de dokunmaz).
		f.fs.flags |= C.EXT2_FLAG_MASTER_SB_ONLY
	}
	return f, nil
}

// writeBlocker yazmayı engelleyen bir durum varsa açıklamasını döndürür.
func (f *FS) writeBlocker() string {
	var info C.lw_info
	C.lw_get_info(f.fs, &info)
	switch {
	case info.feature_incompat&C.EXT3_FEATURE_INCOMPAT_RECOVER != 0:
		return "günlük (journal) kurtarma bekliyor: disk Linux'ta düzgün ayrılmamış. " +
			"Linux'ta bağlayıp ayırın ya da e2fsck çalıştırın"
	case info.state&fsStateError != 0:
		return "dosya sisteminde hata kaydı var: Linux'ta e2fsck -f çalıştırın"
	case info.state&fsStateValid == 0:
		return "dosya sistemi temiz kapatılmamış (belki bu araç yarıda kesildi): Linux'ta e2fsck -f çalıştırın"
	case info.feature_incompat&C.EXT4_FEATURE_INCOMPAT_MMP != 0:
		return "MMP (çoklu bağlama koruması) özelliği desteklenmiyor"
	case info.feature_incompat&C.EXT4_FEATURE_INCOMPAT_ENCRYPT != 0:
		return "fscrypt dizin şifrelemesi açık; yazma desteklenmiyor"
	}
	return ""
}

func (f *FS) err(op string, code C.errcode_t) error {
	if code == 0 {
		return nil
	}
	e := &Error{Op: op, Code: int64(code), Msg: C.GoString(C.error_message(C.long(code)))}
	if code == errEIO && f.io.err != nil {
		e.IO = f.io.err
		f.io.err = nil
	}
	return e
}

func (f *FS) lock() error {
	f.mu.Lock()
	if f.fs == nil || f.closing {
		f.mu.Unlock()
		return ErrClosed
	}
	return nil
}

// closeWait süren yazmaların (yüklemelerin) bitmesi için en fazla bu kadar beklenir.
const closeWait = 15 * time.Second

// Close yeni işlemleri reddeder, süren yüklemelerin iptal edilip temizlenmesini
// bekler, meta veriyi yazar ve dosya sistemini kapatır. Süper blok yalnızca
// hiçbir yazma yarım kalmadıysa "temiz" kalır.
func (f *FS) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fs == nil {
		return nil
	}
	f.closing = true
	// sync.Cond'un zaman aşımı yok: beklerken düzenli uyandır.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				f.cond.Broadcast()
			}
		}
	}()
	deadline := time.Now().Add(closeWait)
	for f.writers > 0 && time.Now().Before(deadline) {
		f.cond.Wait()
	}

	var first error
	if f.writers > 0 {
		// Yarım kalan yazma var. Bellekteki süper blok zaten "temiz değil"
		// durumda; meta veri yine yazılır ama Linux fsck isteyecektir.
		first = fmt.Errorf("%d yazma işlemi yarım kaldı; dosya sistemi kirli işaretli bırakıldı", f.writers)
	}
	if code := C.ext2fs_close_free(&f.fs); code != 0 {
		first = errors.Join(first, f.err("dosya sistemi kapatılamadı", code))
		if f.fs != nil {
			C.ext2fs_free(f.fs)
		}
	}
	f.fs = nil
	// Güvenlik ağı: libext2fs flush çağırmadıysa bekleyen yazmaları boşalt.
	if err := f.io.cache.flushDirty(); err != nil {
		first = errors.Join(first, err)
	}
	f.ioh.Delete()
	return first
}

// Info dosya sistemi özetini döndürür.
func (f *FS) Info() (Info, error) {
	if err := f.lock(); err != nil {
		return Info{}, err
	}
	defer f.mu.Unlock()
	var ci C.lw_info
	C.lw_get_info(f.fs, &ci)
	bs := uint64(ci.block_size)
	free := uint64(0)
	if ci.free_blocks > ci.reserved_blocks {
		free = uint64(ci.free_blocks-ci.reserved_blocks) * bs
	}
	u := C.GoBytes(unsafe.Pointer(&ci.uuid[0]), 16)
	return Info{
		Label:      C.GoString(&ci.label[0]),
		UUID:       fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]),
		BlockSize:  uint32(ci.block_size),
		TotalBytes: uint64(ci.blocks) * bs,
		FreeBytes:  free,
		Inodes:     uint32(ci.inodes),
		FreeInodes: uint32(ci.free_inodes),
		ReadOnly:   f.readOnly,
		ROReason:   f.roReason,
		Features:   featureString(uint32(ci.feature_compat), uint32(ci.feature_incompat), uint32(ci.feature_ro_compat)),
	}, nil
}

func featureString(compat, incompat, ro uint32) string {
	var out []string
	add := func(set uint32, bit uint32, name string) {
		if set&bit != 0 {
			out = append(out, name)
		}
	}
	add(compat, 0x4, "has_journal")
	add(compat, 0x20, "dir_index")
	add(incompat, 0x2, "filetype")
	add(incompat, 0x4, "needs_recovery")
	add(incompat, 0x40, "extent")
	add(incompat, 0x80, "64bit")
	add(incompat, 0x100, "mmp")
	add(incompat, 0x200, "flex_bg")
	add(incompat, 0x8000, "inline_data")
	add(incompat, 0x10000, "encrypt")
	add(incompat, 0x20000, "casefold")
	add(ro, 0x1, "sparse_super")
	add(ro, 0x2, "large_file")
	add(ro, 0x8, "huge_file")
	add(ro, 0x20, "dir_nlink")
	add(ro, 0x40, "extra_isize")
	add(ro, 0x200, "bigalloc")
	add(ro, 0x400, "metadata_csum")
	add(ro, 0x10000, "orphan_present")
	return strings.Join(out, " ")
}

// ---------------- yol çözümleme ----------------

// cleanPath "/a/b" biçimine getirir; ".." ile kök dışına çıkılamaz.
func cleanPath(p string) string {
	return path.Clean("/" + p)
}

func validName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || len(name) > 255 {
		return fmt.Errorf("%w: %q", ErrBadName, name)
	}
	return nil
}

// lookup kilit altında çağrılır. follow: son bileşen sembolik bağsa izle.
func (f *FS) lookup(p string, follow bool) (uint32, error) {
	p = cleanPath(p)
	if p == "/" {
		return C.EXT2_ROOT_INO, nil
	}
	cp := C.CString(strings.TrimPrefix(p, "/"))
	defer C.free(unsafe.Pointer(cp))
	var ino C.ext2_ino_t
	var code C.errcode_t
	if follow {
		code = C.ext2fs_namei_follow(f.fs, C.EXT2_ROOT_INO, C.EXT2_ROOT_INO, cp, &ino)
	} else {
		code = C.ext2fs_namei(f.fs, C.EXT2_ROOT_INO, C.EXT2_ROOT_INO, cp, &ino)
	}
	if code != 0 {
		return 0, f.err(p, code)
	}
	return uint32(ino), nil
}

func (f *FS) stat(ino uint32) (C.lw_stat, error) {
	var st C.lw_stat
	if code := C.lw_stat_ino(f.fs, C.ext2_ino_t(ino), &st); code != 0 {
		return st, f.err("inode okunamadı", code)
	}
	return st, nil
}

func entryFromStat(name string, ino uint32, st C.lw_stat) Entry {
	e := Entry{Name: name, Ino: ino, Mode: uint32(st.mode), Size: uint64(st.size),
		Mtime: time.Unix(int64(st.mtime), 0), UID: uint32(st.uid), GID: uint32(st.gid)}
	switch st.mode & 0xF000 {
	case 0x8000:
		e.Type = "file"
	case 0x4000:
		e.Type = "dir"
		e.Size = 0
	case 0xA000:
		e.Type = "symlink"
	default:
		e.Type = "other"
	}
	return e
}

func (f *FS) readlink(ino uint32) string {
	buf := make([]byte, 4096)
	var n C.size_t
	if C.lw_readlink(f.fs, C.ext2_ino_t(ino), (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &n) != 0 {
		return ""
	}
	return string(buf[:n])
}

// Stat yolu (sembolik bağları izleyerek) çözer.
func (f *FS) Stat(p string) (Entry, error) {
	if err := f.lock(); err != nil {
		return Entry{}, err
	}
	defer f.mu.Unlock()
	ino, err := f.lookup(p, true)
	if err != nil {
		return Entry{}, err
	}
	st, err := f.stat(ino)
	if err != nil {
		return Entry{}, err
	}
	return entryFromStat(path.Base(cleanPath(p)), ino, st), nil
}

// List dizin içeriğini (dizinler önce, ada göre) döndürür.
func (f *FS) List(p string) ([]Entry, error) {
	if err := f.lock(); err != nil {
		return nil, err
	}
	defer f.mu.Unlock()
	ino, err := f.lookup(p, true)
	if err != nil {
		return nil, err
	}
	st, err := f.stat(ino)
	if err != nil {
		return nil, err
	}
	if st.mode&0xF000 != 0x4000 {
		return nil, ErrNotDir
	}
	l := &dirLister{}
	lh := cgo.NewHandle(l)
	code := C.lw_dir_list(f.fs, C.ext2_ino_t(ino), C.uintptr_t(lh))
	lh.Delete()
	if code != 0 {
		return nil, f.err("dizin okunamadı", code)
	}
	out := make([]Entry, 0, len(l.ents))
	for _, d := range l.ents {
		st, err := f.stat(d.ino)
		if err != nil {
			out = append(out, Entry{Name: d.name, Ino: d.ino, Type: "other"})
			continue
		}
		e := entryFromStat(d.name, d.ino, st)
		if e.Type == "symlink" {
			e.Target = f.readlink(d.ino)
			// Bağın gösterdiği şey bir dizinse gezilebilir olsun.
			if t, err := f.lookup(path.Join(cleanPath(p), d.name), true); err == nil {
				if tst, err := f.stat(t); err == nil && tst.mode&0xF000 == 0x4000 {
					e.Type = "dirlink"
				}
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		di := out[i].Type == "dir" || out[i].Type == "dirlink"
		dj := out[j].Type == "dir" || out[j].Type == "dirlink"
		if di != dj {
			return di
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// ---------------- okuma ----------------

// File okuma için açılmış dosya. Her ReadAt kilidi kısa süre tutar, böylece
// uzun indirmeler diğer işlemleri bekletmez.
type File struct {
	fs   *FS
	ino  uint32
	size int64
	pos  int64
	file C.ext2_file_t
}

// OpenFile düzenli bir dosyayı okumak için açar.
func (f *FS) OpenFile(p string) (*File, error) {
	if err := f.lock(); err != nil {
		return nil, err
	}
	defer f.mu.Unlock()
	ino, err := f.lookup(p, true)
	if err != nil {
		return nil, err
	}
	st, err := f.stat(ino)
	if err != nil {
		return nil, err
	}
	if st.mode&0xF000 == 0x4000 {
		return nil, ErrIsDir
	}
	if st.mode&0xF000 != 0x8000 {
		return nil, fmt.Errorf("düzenli dosya değil")
	}
	var file C.ext2_file_t
	if code := C.ext2fs_file_open(f.fs, C.ext2_ino_t(ino), 0, &file); code != 0 {
		return nil, f.err("dosya açılamadı", code)
	}
	f.busy[ino]++
	return &File{fs: f, ino: ino, size: int64(st.size), file: file}, nil
}

func (r *File) Size() int64 { return r.size }

const ioChunk = 1 << 20

func (r *File) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	done := 0
	for done < len(p) && off+int64(done) < r.size {
		// Dosya sonunu aşan istek verme: inline_data okuması fazlasını sıfırla
		// doldurup okunmuş sayıyor.
		n := int(min(int64(len(p)-done), ioChunk, r.size-off-int64(done)))
		got, err := r.readChunk(p[done:done+n], off+int64(done))
		done += got
		if err != nil {
			return done, err
		}
		if got == 0 {
			break
		}
	}
	if done < len(p) {
		return done, io.EOF
	}
	return done, nil
}

func (r *File) readChunk(p []byte, off int64) (int, error) {
	if err := r.fs.lock(); err != nil {
		return 0, err
	}
	defer r.fs.mu.Unlock()
	if r.file == nil {
		return 0, ErrClosed
	}
	if code := C.ext2fs_file_llseek(r.file, C.__u64(off), C.EXT2_SEEK_SET, nil); code != 0 {
		return 0, r.fs.err("konumlanamadı", code)
	}
	var got C.uint
	if code := C.ext2fs_file_read(r.file, unsafe.Pointer(&p[0]), C.uint(len(p)), &got); code != 0 {
		return int(got), r.fs.err("okunamadı", code)
	}
	return int(got), nil
}

func (r *File) Read(p []byte) (int, error) {
	n, err := r.ReadAt(p, r.pos)
	r.pos += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

func (r *File) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += r.pos
	case io.SeekEnd:
		off += r.size
	}
	if off < 0 {
		return 0, errors.New("negatif konum")
	}
	r.pos = off
	return off, nil
}

func (r *File) Close() error {
	r.fs.mu.Lock()
	defer r.fs.mu.Unlock()
	if r.file == nil {
		return nil
	}
	var err error
	if r.fs.fs != nil {
		if code := C.ext2fs_file_close(r.file); code != 0 {
			err = r.fs.err("kapatılamadı", code)
		}
	}
	r.file = nil
	if r.fs.busy[r.ino]--; r.fs.busy[r.ino] <= 0 {
		delete(r.fs.busy, r.ino)
	}
	return err
}
