package blockdev_test

import (
	"bytes"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"luks_web/internal/blockdev"
	"luks_web/internal/ext2fs"
	"luks_web/internal/luks2"
	"luks_web/internal/testimg"
)

// Hedef disk düzeni: GPT, bölüm 1 exFAT "TOOLS", bölüm 2 LUKS2 -> ext4.
func TestGPTLayout(t *testing.T) {
	testimg.Need(t, "sfdisk")
	dir := t.TempDir()
	_, enc := testimg.Make(t, dir, testimg.Options{SizeMB: 32})
	encData, err := os.ReadFile(enc)
	if err != nil {
		t.Fatal(err)
	}

	const mb = 1 << 20
	disk := filepath.Join(dir, "disk.img")
	f, err := os.Create(disk)
	if err != nil {
		t.Fatal(err)
	}
	p2off := int64(10 * mb)
	if err := f.Truncate(p2off + int64(len(encData)) + mb); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cmd := exec.Command("sfdisk", "--no-reread", "--no-tell-kernel", "-q", disk)
	cmd.Stdin = strings.NewReader(
		"label: gpt\n" +
			"start=2048, size=16384, type=EBD0A0A2-B9E5-4433-87C0-68B6B72699C7, name=TOOLS\n" +
			"start=20480, size=" + strconv.Itoa(len(encData)/512) + ", type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name=VERI\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sfdisk: %v\n%s", err, out)
	}
	if mk, err := exec.LookPath("mkfs.exfat"); err == nil {
		// Bölüm 1'i gerçekten exFAT yap (ayrı dosyada, sonra kopyala).
		p1 := filepath.Join(dir, "p1.img")
		os.WriteFile(p1, nil, 0o600)
		os.Truncate(p1, 8*mb)
		if out, err := exec.Command(mk, "-L", "TOOLS", p1).CombinedOutput(); err != nil {
			t.Fatalf("mkfs.exfat: %v\n%s", err, out)
		}
		writeAt(t, disk, mustRead(t, p1), mb)
	}
	writeAt(t, disk, encData, p2off)

	cands, err := blockdev.ScanPath(disk)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Partition != 2 || cands[0].Offset != p2off || cands[0].PartName != "VERI" {
		t.Fatalf("adaylar: %+v", cands)
	}

	dev, err := blockdev.OpenCandidate(cands[0], true)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	hdr, err := luks2.ReadHeader(dev)
	if err != nil {
		t.Fatal(err)
	}
	mk, err := hdr.Unlock(dev, []byte(testimg.Passphrase))
	if err != nil {
		t.Fatal(err)
	}
	vol, err := hdr.OpenVolume(dev, mk)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := ext2fs.Open(vol, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Create("/gpt.txt", strings.NewReader("bölüm 2"), 0, false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}

	// Bölüm 1 bozulmamış olmalı, bölüm 2 dışına yazılmamış olmalı.
	all := mustRead(t, disk)
	if !bytes.Equal(all[p2off : p2off+int64(len(encData))][:4096], encData[:4096]) {
		t.Fatal("LUKS başlığı değişmiş")
	}
	part := filepath.Join(dir, "p2.img")
	os.WriteFile(part, all[p2off:p2off+int64(len(encData))], 0o600)
	testimg.Fsck(t, testimg.Decrypt(t, part))
}

// Section sınır dışına taşmamalı.
func TestSectionBounds(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "d.img")
	data := make([]byte, 64<<10)
	rand.Read(data)
	os.WriteFile(p, data, 0o600)
	dev, err := blockdev.Open(p, true)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	s := blockdev.NewSection(dev, 4096, 8192)
	b := make([]byte, 100)
	if _, err := s.ReadAt(b, 8100); err == nil {
		t.Fatal("sınır dışı okuma kabul edildi")
	}
	if _, err := s.WriteAt(b, 8150); err == nil {
		t.Fatal("sınır dışı yazma kabul edildi")
	}
	if _, err := s.ReadAt(b, 10); err != nil || !bytes.Equal(b, data[4106:4206]) {
		t.Fatal("bölüm okuması yanlış")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeAt(t *testing.T, p string, data []byte, off int64) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(data, off); err != nil {
		t.Fatal(err)
	}
}
