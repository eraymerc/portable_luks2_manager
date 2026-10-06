package ext2fs_test

import (
	"io"
	"testing"
	"time"

	"luks_web/internal/testimg"
)

type zeroReader struct{ n int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), z.n))
	clear(p[:n])
	z.n -= int64(n)
	return n, nil
}

// go test -run x -bench Upload -benchtime 1x ./internal/ext2fs
func BenchmarkUpload(b *testing.B) {
	t := &testing.T{}
	testimg.Need(b)
	_, enc := testimg.Make(b, b.TempDir(), testimg.Options{SizeMB: 600})
	const size = 400 << 20
	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := open(t, enc, true)
		if _, err := s.fs.Create("/b.bin", &zeroReader{n: size}, size, false, time.Time{}); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		s.fs.Remove("/b.bin", false)
		s.close(t)
		b.StartTimer()
	}
}

func BenchmarkDownload(b *testing.B) {
	t := &testing.T{}
	testimg.Need(b)
	_, enc := testimg.Make(b, b.TempDir(), testimg.Options{SizeMB: 600})
	const size = 400 << 20
	s := open(t, enc, true)
	s.fs.Create("/b.bin", &zeroReader{n: size}, size, false, time.Time{})
	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, _ := s.fs.OpenFile("/b.bin")
		io.Copy(io.Discard, f)
		f.Close()
	}
	b.StopTimer()
	s.close(t)
}
