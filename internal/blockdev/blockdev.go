// Package blockdev ham aygıt erişimi sağlar: imaj dosyası, Linux blok aygıtı
// veya Windows \\.\PhysicalDriveN; ayrıca GPT/MBR bölüm tablosunu okur.
package blockdev

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Device rastgele erişimli ham aygıttır.
type Device interface {
	io.ReaderAt
	io.WriterAt
	Size() int64
	Sync() error
	Close() error
}

// fileDev düz dosya (imaj) veya Linux blok aygıtı.
type fileDev struct {
	f    *os.File
	size int64
}

func (d *fileDev) ReadAt(p []byte, off int64) (int, error)  { return d.f.ReadAt(p, off) }
func (d *fileDev) WriteAt(p []byte, off int64) (int, error) { return d.f.WriteAt(p, off) }
func (d *fileDev) Size() int64                              { return d.size }
func (d *fileDev) Sync() error                              { return d.f.Sync() }
func (d *fileDev) Close() error                             { return d.f.Close() }

func openFile(path string, rw bool) (Device, error) {
	flag := os.O_RDONLY
	if rw {
		flag = os.O_RDWR
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return nil, err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		f.Close()
		return nil, err
	}
	if size == 0 {
		f.Close()
		return nil, fmt.Errorf("%s: boyut 0", path)
	}
	return &fileDev{f: f, size: size}, nil
}

// Section bir aygıtın [off, off+size) aralığını ayrı aygıt gibi gösterir.
// Kapatılınca alttaki aygıtı da kapatır.
type Section struct {
	dev       Device
	off, size int64
}

func NewSection(dev Device, off, size int64) *Section {
	return &Section{dev: dev, off: off, size: size}
}

func (s *Section) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > s.size {
		return 0, io.ErrUnexpectedEOF
	}
	return s.dev.ReadAt(p, s.off+off)
}

func (s *Section) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > s.size {
		return 0, errors.New("bölüm sınırı dışına yazma")
	}
	return s.dev.WriteAt(p, s.off+off)
}

func (s *Section) Size() int64  { return s.size }
func (s *Section) Sync() error  { return s.dev.Sync() }
func (s *Section) Close() error { return s.dev.Close() }

// aligned alttaki aygıta yalnız align'ın katı ofset/uzunlukla erişir
// (Windows ham diskleri bunu şart koşar). Hizasız yazmalar oku-değiştir-yaz olur.
type aligned struct {
	Device
	align int64
}

func (a *aligned) ReadAt(p []byte, off int64) (int, error) {
	if off%a.align == 0 && int64(len(p))%a.align == 0 {
		return a.Device.ReadAt(p, off)
	}
	start := off &^ (a.align - 1)
	end := (off + int64(len(p)) + a.align - 1) &^ (a.align - 1)
	if end > a.Size() {
		end = a.Size()
	}
	buf := make([]byte, end-start)
	if _, err := a.Device.ReadAt(buf, start); err != nil {
		return 0, err
	}
	n := copy(p, buf[off-start:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (a *aligned) WriteAt(p []byte, off int64) (int, error) {
	if off%a.align == 0 && int64(len(p))%a.align == 0 {
		return a.Device.WriteAt(p, off)
	}
	start := off &^ (a.align - 1)
	end := (off + int64(len(p)) + a.align - 1) &^ (a.align - 1)
	buf := make([]byte, end-start)
	if _, err := a.Device.ReadAt(buf, start); err != nil {
		return 0, err
	}
	copy(buf[off-start:], p)
	if _, err := a.Device.WriteAt(buf, start); err != nil {
		return 0, err
	}
	return len(p), nil
}
