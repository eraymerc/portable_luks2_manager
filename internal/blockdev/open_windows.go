//go:build windows

package blockdev

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	ioctlDiskGetLengthInfo    = 0x0007405C
	ioctlStorageQueryProperty = 0x002D1400
	ioctlDiskGetDriveGeometry = 0x00070000
	maxPhysicalDrives         = 32
	rawAlign                  = 4096 // 512 ve 4096 mantıksal sektörün katı
)

func isRawPath(p string) bool { return strings.HasPrefix(p, `\\.\`) || strings.HasPrefix(p, `//./`) }

// Open \\.\PhysicalDriveN yollarını ham disk olarak, diğerlerini dosya olarak açar.
func Open(path string, rw bool) (Device, error) {
	if !isRawPath(path) {
		return openFile(path, rw)
	}
	access := uint32(windows.GENERIC_READ)
	if rw {
		access |= windows.GENERIC_WRITE
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if err == windows.ERROR_ACCESS_DENIED {
			return nil, fmt.Errorf("%s: erişim reddedildi (yönetici olarak çalıştırın)", path)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var length int64
	var ret uint32
	if err := windows.DeviceIoControl(h, ioctlDiskGetLengthInfo, nil, 0,
		(*byte)(unsafe.Pointer(&length)), 8, &ret, nil); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("%s: disk boyutu alınamadı: %w", path, err)
	}
	f := os.NewFile(uintptr(h), path)
	return &aligned{Device: &fileDev{f: f, size: length}, align: rawAlign}, nil
}

func physicalDrives() []string {
	var out []string
	for i := 0; i < maxPhysicalDrives; i++ {
		p := fmt.Sprintf(`\\.\PhysicalDrive%d`, i)
		ptr, _ := windows.UTF16PtrFromString(p)
		h, err := windows.CreateFile(ptr, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			continue
		}
		windows.CloseHandle(h)
		out = append(out, p)
	}
	return out
}

// describe disk üretici/model bilgisini döndürür (erişim hakkı gerekmez).
func describe(path string) string {
	if !isRawPath(path) {
		return ""
	}
	ptr, _ := windows.UTF16PtrFromString(path)
	h, err := windows.CreateFile(ptr, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	query := make([]byte, 12) // PropertyId=StorageDeviceProperty(0), QueryType=Standard(0)
	out := make([]byte, 1024)
	var ret uint32
	if err := windows.DeviceIoControl(h, ioctlStorageQueryProperty, &query[0], uint32(len(query)),
		&out[0], uint32(len(out)), &ret, nil); err != nil || ret < 32 {
		return ""
	}
	out = out[:ret]
	str := func(off uint32) string {
		if off == 0 || int(off) >= len(out) {
			return ""
		}
		b := out[off:]
		for i, c := range b {
			if c == 0 {
				b = b[:i]
				break
			}
		}
		return strings.TrimSpace(string(b))
	}
	le := binary.LittleEndian
	vendor, product := str(le.Uint32(out[12:])), str(le.Uint32(out[16:]))
	bus := map[uint32]string{7: "USB", 11: "SATA", 17: "NVMe", 8: "RAID", 10: "SAS", 1: "SCSI", 3: "ATA"}[le.Uint32(out[28:])]
	s := strings.TrimSpace(vendor + " " + product)
	if bus != "" {
		s += " (" + bus + ")"
	}
	return s
}
