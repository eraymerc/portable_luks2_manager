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
	"time"
	"unsafe"
)

// beginWrite kilit altında çağrılır: yazmaya izin var mı bakar, bitmap'leri
// yükler ve (sürmekte olan başka yazma yoksa) süper bloğu "temiz değil" diye
// işaretleyip diske yazar. Her beginWrite'a bir endWrite karşılık gelmelidir.
//
// Süper blok yalnızca bir işlem sürerken kirli kalır: işlem bitince meta veri
// yazılır ve yeniden "temiz" işaretlenir. Böylece disk boştayken çekilirse
// sorun olmaz; bir işlemin ortasında kesilirse Linux fsck ister.
func (f *FS) beginWrite() error {
	if f.readOnly {
		return fmt.Errorf("%w: %s", ErrReadOnly, f.roReason)
	}
	if !f.bitmaps {
		if code := C.ext2fs_read_bitmaps(f.fs); code != 0 {
			return f.err("bitmap'ler okunamadı", code)
		}
		f.bitmaps = true
	}
	if !f.marked {
		var info C.lw_info
		C.lw_get_info(f.fs, &info)
		C.lw_set_state(f.fs, info.state&^fsStateValid)
		if err := f.flush(); err != nil {
			return err
		}
		f.marked = true
	}
	f.writers++
	return nil
}

// endWrite meta veriyi yazar; son yazıcıysa süper bloğu yeniden temiz işaretler.
// Yazma başarısız olursa süper blok kirli kalır (fsck istenir).
func (f *FS) endWrite() error {
	f.writers--
	defer f.cond.Broadcast()
	if err := f.flush(); err != nil {
		return err
	}
	if f.writers > 0 {
		return nil
	}
	var info C.lw_info
	C.lw_get_info(f.fs, &info)
	C.lw_set_state(f.fs, info.state|fsStateValid)
	if err := f.flush(); err != nil {
		return err
	}
	f.marked = false
	return nil
}

// flush meta veriyi (grup tanımlayıcıları, bitmap'ler, süper blok) diske yazar.
func (f *FS) flush() error {
	if code := C.ext2fs_flush(f.fs); code != 0 {
		return f.err("diske yazılamadı", code)
	}
	return nil
}

// parentOf kilit altında: p'nin üst dizininin inode'u ve son bileşen.
func (f *FS) parentOf(p string) (uint32, string, error) {
	p = cleanPath(p)
	if p == "/" {
		return 0, "", fmt.Errorf("%w: kök dizin", ErrBadName)
	}
	dir, name := path.Split(p)
	if err := validName(name); err != nil {
		return 0, "", err
	}
	pino, err := f.lookup(dir, true)
	if err != nil {
		return 0, "", err
	}
	st, err := f.stat(pino)
	if err != nil {
		return 0, "", err
	}
	if st.mode&0xF000 != 0x4000 {
		return 0, "", ErrNotDir
	}
	return pino, name, nil
}

// exists kilit altında: dizinde ad var mı.
func (f *FS) exists(dir uint32, name string) (uint32, bool, error) {
	cn := C.CString(name)
	defer C.free(unsafe.Pointer(cn))
	var ino C.ext2_ino_t
	code := C.ext2fs_lookup(f.fs, C.ext2_ino_t(dir), cn, C.int(len(name)), nil, &ino)
	if code == C.EXT2_ET_FILE_NOT_FOUND {
		return 0, false, nil
	}
	if code != 0 {
		return 0, false, f.err("arama", code)
	}
	return uint32(ino), true, nil
}

// spaceFor yazılacak n bayt için (kök ayrılmış alanı hariç) yer var mı.
func (f *FS) spaceFor(n int64) error {
	var info C.lw_info
	C.lw_get_info(f.fs, &info)
	bs := int64(info.block_size)
	need := (n+bs-1)/bs + 16 // + biraz meta veri payı
	avail := int64(info.free_blocks) - int64(info.reserved_blocks)
	if avail < need {
		return ErrNoSpace
	}
	return nil
}

// Mkdir yeni bir dizin oluşturur; sahibi üst dizinin sahibi olur.
func (f *FS) Mkdir(p string) (err error) {
	if err := f.lock(); err != nil {
		return err
	}
	defer f.mu.Unlock()
	if err := f.beginWrite(); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.endWrite()) }()
	pino, name, err := f.parentOf(p)
	if err != nil {
		return err
	}
	if _, ok, err := f.exists(pino, name); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%s: %w", name, ErrExists)
	}
	if err := f.spaceFor(0); err != nil {
		return err
	}
	pst, err := f.stat(pino)
	if err != nil {
		return err
	}
	cn := C.CString(name)
	defer C.free(unsafe.Pointer(cn))
	var ino C.ext2_ino_t
	if code := C.lw_mkdir(f.fs, C.ext2_ino_t(pino), cn, 0o755, pst.uid, pst.gid, &ino); code != 0 {
		return f.err("dizin oluşturulamadı", code)
	}
	return nil
}

// Create p yolunda yeni bir dosya oluşturur ve r'nin içeriğini yazar. Dosya
// varsa ve overwrite=true ise içerik önce geçici bir ada yazılır, başarılı
// olunca eskisinin yerine konur. Hata olursa yarım dosya silinir.
// mtime sıfır değilse dosyanın değişiklik zamanı olarak atanır.
func (f *FS) Create(p string, r io.Reader, sizeHint int64, overwrite bool, mtime time.Time) (int64, error) {
	pino, name, tmpName, oldIno, exists, err := f.prepareCreate(p, sizeHint, overwrite)
	if err != nil {
		return 0, err
	}
	// prepareCreate başarılıysa kilit tutuluyor ve beginWrite yapılmış durumda.
	cn := C.CString(tmpName)
	defer C.free(unsafe.Pointer(cn))
	pst, err := f.stat(pino)
	if err != nil {
		err = errors.Join(err, f.endWrite())
		f.mu.Unlock()
		return 0, err
	}
	var ino C.ext2_ino_t
	if code := C.lw_create_file(f.fs, C.ext2_ino_t(pino), cn, 0o644, pst.uid, pst.gid, &ino); code != 0 {
		err := errors.Join(f.err("dosya oluşturulamadı", code), f.endWrite())
		f.mu.Unlock()
		return 0, err
	}
	var file C.ext2_file_t
	if code := C.ext2fs_file_open(f.fs, ino, C.EXT2_FILE_WRITE, &file); code != 0 {
		err := f.err("dosya açılamadı", code)
		C.lw_remove(f.fs, C.ext2_ino_t(pino), cn, ino)
		err = errors.Join(err, f.endWrite())
		f.mu.Unlock()
		return 0, err
	}
	f.busy[uint32(ino)]++
	f.mu.Unlock()

	// Veriyi parça parça yaz; kilit yalnız her parçanın yazımı sırasında tutulur.
	var written int64
	buf := make([]byte, ioChunk)
	var werr error
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			if werr = f.writeChunk(file, buf[:n]); werr != nil {
				break
			}
			written += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			werr = rerr
			break
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fs == nil {
		return written, ErrClosed
	}
	if code := C.ext2fs_file_close(file); code != 0 && werr == nil {
		werr = f.err("dosya kapatılamadı", code)
	}
	if f.busy[uint32(ino)]--; f.busy[uint32(ino)] <= 0 {
		delete(f.busy, uint32(ino))
	}
	if werr != nil {
		// Yarım dosyayı sil.
		if code := C.lw_remove(f.fs, C.ext2_ino_t(pino), cn, ino); code != 0 {
			werr = errors.Join(werr, f.err("yarım dosya silinemedi", code))
		}
		return written, errors.Join(werr, f.endWrite())
	}
	if !mtime.IsZero() {
		C.lw_set_mtime(f.fs, ino, C.int64_t(mtime.Unix()))
	}
	if exists {
		// Eskisini kaldır, geçiciyi asıl ada taşı.
		on := C.CString(name)
		defer C.free(unsafe.Pointer(on))
		if code := C.lw_remove(f.fs, C.ext2_ino_t(pino), on, C.ext2_ino_t(oldIno)); code != 0 {
			return written, errors.Join(f.err("eski dosya silinemedi", code), f.endWrite())
		}
		if code := C.lw_rename(f.fs, C.ext2_ino_t(pino), cn, C.ext2_ino_t(pino), on, ino); code != 0 {
			return written, errors.Join(f.err("yeniden adlandırılamadı", code), f.endWrite())
		}
	}
	return written, f.endWrite()
}

// prepareCreate denetimleri yapar. Başarılıysa kilidi TUTARAK ve beginWrite
// yapılmış olarak döner; hata durumunda kilit bırakılmıştır.
func (f *FS) prepareCreate(p string, sizeHint int64, overwrite bool) (pino uint32, name, tmpName string, oldIno uint32, exists bool, err error) {
	if err = f.lock(); err != nil {
		return
	}
	defer func() {
		if err != nil {
			f.mu.Unlock()
		}
	}()
	if pino, name, err = f.parentOf(p); err != nil {
		return
	}
	if oldIno, exists, err = f.exists(pino, name); err != nil {
		return
	}
	if exists {
		var ost C.lw_stat
		if ost, err = f.stat(oldIno); err != nil {
			return
		}
		if !overwrite || ost.mode&0xF000 != 0x8000 {
			err = fmt.Errorf("%s: %w", name, ErrExists)
			return
		}
		if f.busy[oldIno] > 0 {
			err = fmt.Errorf("%s: %w", name, ErrBusy)
			return
		}
	}
	if err = f.beginWrite(); err != nil {
		return
	}
	if sizeHint > 0 {
		if err = f.spaceFor(sizeHint); err != nil {
			err = errors.Join(err, f.endWrite())
			return
		}
	}
	tmpName = name
	if exists {
		tmpName = fmt.Sprintf(".%s.luksweb-%d", name, time.Now().UnixNano())
		if len(tmpName) > 255 {
			tmpName = fmt.Sprintf(".luksweb-%d", time.Now().UnixNano())
		}
	}
	return
}

func (f *FS) writeChunk(file C.ext2_file_t, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fs == nil || f.closing {
		return ErrClosed
	}
	if err := f.spaceFor(int64(len(p))); err != nil {
		return err
	}
	var wrote C.uint
	if code := C.ext2fs_file_write(file, unsafe.Pointer(&p[0]), C.uint(len(p)), &wrote); code != 0 {
		return f.err("yazılamadı", code)
	}
	if int(wrote) != len(p) {
		return fmt.Errorf("kısa yazma (%d/%d)", wrote, len(p))
	}
	return nil
}

// Remove dosyayı, sembolik bağı ya da (recursive ise içiyle birlikte) dizini siler.
func (f *FS) Remove(p string, recursive bool) (err error) {
	if err := f.lock(); err != nil {
		return err
	}
	defer f.mu.Unlock()
	if err := f.beginWrite(); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.endWrite()) }()
	pino, name, err := f.parentOf(p)
	if err != nil {
		return err
	}
	ino, ok, err := f.exists(pino, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return f.removeLocked(pino, name, ino, recursive, 0)
}

func (f *FS) removeLocked(pino uint32, name string, ino uint32, recursive bool, depth int) error {
	if depth > 4096 {
		return errors.New("dizin ağacı çok derin")
	}
	if f.busy[ino] > 0 {
		return fmt.Errorf("%s: %w", name, ErrBusy)
	}
	if ino < uint32(f.fs.super.s_first_ino) {
		return fmt.Errorf("%s: sistem inode'u silinemez", name)
	}
	st, err := f.stat(ino)
	if err != nil {
		return err
	}
	if st.mode&0xF000 == 0x4000 {
		var empty C.int
		if code := C.lw_dir_is_empty(f.fs, C.ext2_ino_t(ino), &empty); code != 0 {
			return f.err("dizin okunamadı", code)
		}
		if empty == 0 {
			if !recursive {
				return fmt.Errorf("%s: %w", name, ErrNotEmpty)
			}
			l := &dirLister{}
			lh := cgo.NewHandle(l)
			code := C.lw_dir_list(f.fs, C.ext2_ino_t(ino), C.uintptr_t(lh))
			lh.Delete()
			if code != 0 {
				return f.err("dizin okunamadı", code)
			}
			for _, d := range l.ents {
				if err := f.removeLocked(ino, d.name, d.ino, true, depth+1); err != nil {
					return err
				}
			}
		}
	}
	cn := C.CString(name)
	defer C.free(unsafe.Pointer(cn))
	if code := C.lw_remove(f.fs, C.ext2_ino_t(pino), cn, C.ext2_ino_t(ino)); code != 0 {
		return f.err(name+" silinemedi", code)
	}
	return nil
}

// Rename from'u to'ya taşır/yeniden adlandırır. Hedef varsa hata verir.
func (f *FS) Rename(from, to string) (err error) {
	if err := f.lock(); err != nil {
		return err
	}
	defer f.mu.Unlock()
	if err := f.beginWrite(); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.endWrite()) }()
	opino, oname, err := f.parentOf(from)
	if err != nil {
		return err
	}
	ino, ok, err := f.exists(opino, oname)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s: %w", oname, ErrNotFound)
	}
	npino, nname, err := f.parentOf(to)
	if err != nil {
		return err
	}
	if _, ok, err := f.exists(npino, nname); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%s: %w", nname, ErrExists)
	}
	st, err := f.stat(ino)
	if err != nil {
		return err
	}
	if st.mode&0xF000 == 0x4000 && npino != opino {
		// Dizini kendi alt ağacına taşımayı engelle.
		for cur := npino; cur != C.EXT2_ROOT_INO; {
			if cur == ino {
				return errors.New("dizin kendi içine taşınamaz")
			}
			var parent C.ext2_ino_t
			if code := C.lw_dotdot(f.fs, C.ext2_ino_t(cur), &parent); code != 0 {
				return f.err("üst dizin bulunamadı", code)
			}
			if uint32(parent) == cur {
				break
			}
			cur = uint32(parent)
		}
	}
	co, cn := C.CString(oname), C.CString(nname)
	defer C.free(unsafe.Pointer(co))
	defer C.free(unsafe.Pointer(cn))
	if code := C.lw_rename(f.fs, C.ext2_ino_t(opino), co, C.ext2_ino_t(npino), cn, C.ext2_ino_t(ino)); code != 0 {
		return f.err("taşınamadı", code)
	}
	return nil
}
