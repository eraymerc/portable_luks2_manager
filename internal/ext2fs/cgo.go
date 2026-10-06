package ext2fs

/*
#cgo CFLAGS: -O2 -Wall -Wno-unused-parameter -Wno-unused-function
#cgo linux CFLAGS: -I${SRCDIR}/../../build/e2fs/linux/include
#cgo linux LDFLAGS: ${SRCDIR}/../../build/e2fs/linux/lib/libext2fs.a ${SRCDIR}/../../build/e2fs/linux/lib/libcom_err.a -lpthread
#cgo windows CFLAGS: -I${SRCDIR}/../../build/e2fs/windows/include -I${SRCDIR}/../../build/e2fs/windows/include/mingw
#cgo windows LDFLAGS: ${SRCDIR}/../../build/e2fs/windows/lib/libext2fs.a ${SRCDIR}/../../build/e2fs/windows/lib/libcom_err.a -lws2_32
#include "lw.h"
*/
import "C"

import (
	"runtime/cgo"
	"unsafe"
)

// ioState bir io kanalının Go tarafı.
type ioState struct {
	cache    *chunkCache
	readOnly bool
	err      error // son G/Ç hatası (errcode yalnız EIO taşır)
}

const errEIO = 5 // EIO; com_err errno aralığı

//export lwRead
func lwRead(h C.uintptr_t, off C.ulonglong, buf unsafe.Pointer, n C.size_t) C.errcode_t {
	s := cgo.Handle(h).Value().(*ioState)
	p := unsafe.Slice((*byte)(buf), int(n))
	if _, err := s.cache.ReadAt(p, int64(off)); err != nil {
		s.err = err
		return errEIO
	}
	return 0
}

//export lwWrite
func lwWrite(h C.uintptr_t, off C.ulonglong, buf unsafe.Pointer, n C.size_t) C.errcode_t {
	s := cgo.Handle(h).Value().(*ioState)
	if s.readOnly {
		return C.EXT2_ET_RO_FILSYS
	}
	p := unsafe.Slice((*byte)(buf), int(n))
	if _, err := s.cache.WriteAt(p, int64(off)); err != nil {
		s.err = err
		return errEIO
	}
	return 0
}

//export lwFlush
func lwFlush(h C.uintptr_t) C.errcode_t {
	s := cgo.Handle(h).Value().(*ioState)
	if s.readOnly {
		return 0
	}
	if err := s.cache.Sync(); err != nil {
		s.err = err
		return errEIO
	}
	return 0
}

// dirLister dizin listesi toplar.
type dirLister struct {
	ents []dirent
}

type dirent struct {
	name  string
	ino   uint32
	ftype int
}

//export lwDirEntry
func lwDirEntry(priv C.uintptr_t, name *C.char, n C.int, ino C.ext2_ino_t, ftype C.int) C.int {
	l := cgo.Handle(priv).Value().(*dirLister)
	l.ents = append(l.ents, dirent{name: C.GoStringN(name, n), ino: uint32(ino), ftype: int(ftype)})
	return 0
}
