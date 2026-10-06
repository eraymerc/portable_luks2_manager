package blockdev

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"testing"
)

// strictDev hizasız erişimde hata verir (Windows ham diski taklit eder).
type strictDev struct {
	data  []byte
	align int64
}

func (d *strictDev) check(n int, off int64) error {
	if off%d.align != 0 || int64(n)%d.align != 0 {
		return fmt.Errorf("hizasız erişim off=%d n=%d", off, n)
	}
	return nil
}
func (d *strictDev) ReadAt(p []byte, off int64) (int, error) {
	if err := d.check(len(p), off); err != nil {
		return 0, err
	}
	return copy(p, d.data[off:]), nil
}
func (d *strictDev) WriteAt(p []byte, off int64) (int, error) {
	if err := d.check(len(p), off); err != nil {
		return 0, err
	}
	return copy(d.data[off:], p), nil
}
func (d *strictDev) Size() int64  { return int64(len(d.data)) }
func (d *strictDev) Sync() error  { return nil }
func (d *strictDev) Close() error { return nil }

func TestAligned(t *testing.T) {
	ref := make([]byte, 64<<10)
	rand.Read(ref)
	dev := &strictDev{data: append([]byte(nil), ref...), align: 512}
	a := &aligned{Device: dev, align: 4096}

	cases := []struct{ off, n int64 }{{0, 6}, {510, 3000}, {4095, 2}, {8192, 4096}, {100, 20000}, {65530, 6}}
	for _, c := range cases {
		got := make([]byte, c.n)
		if _, err := a.ReadAt(got, c.off); err != nil {
			t.Fatalf("okuma %+v: %v", c, err)
		}
		if !bytes.Equal(got, ref[c.off:c.off+c.n]) {
			t.Fatalf("okuma %+v yanlış", c)
		}
		w := make([]byte, c.n)
		rand.Read(w)
		if _, err := a.WriteAt(w, c.off); err != nil {
			t.Fatalf("yazma %+v: %v", c, err)
		}
		copy(ref[c.off:], w)
		if !bytes.Equal(dev.data, ref) {
			t.Fatalf("yazma %+v çevredeki veriyi bozdu", c)
		}
	}
}

func TestReadPartitionsMBR(t *testing.T) {
	d := &strictDev{data: make([]byte, 8<<20), align: 1}
	m := d.data
	m[510], m[511] = 0x55, 0xaa
	e := m[446:]
	e[4] = 0x83
	e[8], e[9] = 0x00, 0x08   // LBA 2048
	e[12], e[13] = 0x00, 0x10 // 4096 sektör
	parts, err := ReadPartitions(d, d.Size())
	if err != nil || len(parts) != 1 || parts[0].Offset != 2048*512 || parts[0].Size != 4096*512 {
		t.Fatalf("%+v %v", parts, err)
	}
}
