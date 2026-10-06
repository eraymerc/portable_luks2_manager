//go:build !windows

package blockdev

// Open imaj dosyasını veya blok aygıtını açar.
func Open(path string, rw bool) (Device, error) { return openFile(path, rw) }

func physicalDrives() []string { return nil }

func describe(path string) string { return "" }
