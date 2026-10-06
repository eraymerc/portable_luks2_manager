package ext2fs_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"luks_web/internal/blockdev"
	"luks_web/internal/ext2fs"
	"luks_web/internal/luks2"
	"luks_web/internal/testimg"
)

// populate mkfs.ext4 -d için örnek ağaç.
func populate(t *testing.T) (string, map[string][]byte) {
	dir := t.TempDir()
	files := map[string][]byte{
		"a.txt":                []byte("merhaba dünya\n"),
		"Türkçe ad ğüşıöç.txt": []byte("içerik"),
		"alt/rand.bin":         randBytes(3_000_123),
		"alt/bos":              {},
	}
	for i := 0; i < 300; i++ { // htree'li büyük dizin
		files[fmt.Sprintf("alt/çok/f%03d.txt", i)] = []byte(fmt.Sprint(i))
	}
	for name, data := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.Symlink("a.txt", filepath.Join(dir, "link"))
	os.Symlink("alt", filepath.Join(dir, "dirlink"))
	return dir, files
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

type session struct {
	dev blockdev.Device
	fs  *ext2fs.FS
}

func open(t *testing.T, enc string, writable bool) *session {
	t.Helper()
	dev, err := blockdev.Open(enc, writable)
	if err != nil {
		t.Fatal(err)
	}
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
	fs, err := ext2fs.Open(vol, writable)
	if err != nil {
		t.Fatal(err)
	}
	return &session{dev, fs}
}

func (s *session) close(t *testing.T) {
	t.Helper()
	if err := s.fs.Close(); err != nil {
		t.Fatal(err)
	}
	s.dev.Close()
}

func readAll(t *testing.T, fs *ext2fs.FS, p string) []byte {
	t.Helper()
	f, err := fs.OpenFile(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return b
}

func debugfs(t *testing.T, img, cmd string) []byte {
	t.Helper()
	out, err := exec.Command("debugfs", "-R", cmd, img).Output()
	if err != nil {
		t.Fatalf("debugfs %q: %v", cmd, err)
	}
	return out
}

var sizeRe = regexp.MustCompile(`\bSize: (\d+)`)

var variants = []struct {
	name string
	opts testimg.Options
}{
	{"ext4-varsayılan", testimg.Options{}},
	{"ext4-1k-blok", testimg.Options{BlockSize: 1024, SectorSize: 512}}, // cryptsetup 4K sektör + 1K blok kabul etmiyor
	{"ext4-inline_data", testimg.Options{MkfsArgs: []string{"-O", "inline_data"}}},
	{"ext2-günlüksüz", testimg.Options{MkfsArgs: []string{"-t", "ext2"}}},
	{"luks-512-sektör", testimg.Options{SectorSize: 512}},
}

func TestReadWrite(t *testing.T) {
	testimg.Need(t, "e2fsck", "debugfs")
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			src, files := populate(t)
			o := v.opts
			o.Populate = src
			_, enc := testimg.Make(t, t.TempDir(), o)

			// --- okuma ---
			s := open(t, enc, false)
			for name, want := range files {
				if got := readAll(t, s.fs, name); !bytes.Equal(got, want) {
					t.Fatalf("%s: içerik farklı (%d/%d bayt)", name, len(got), len(want))
				}
			}
			ents, err := s.fs.List("/")
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, e := range ents {
				names = append(names, e.Name+":"+e.Type)
				if e.Name == "link" && e.Target != "a.txt" {
					t.Fatalf("link hedefi %q", e.Target)
				}
			}
			sort.Strings(names)
			want := "Türkçe ad ğüşıöç.txt:file a.txt:file alt:dir dirlink:dirlink link:symlink lost+found:dir"
			if got := strings.Join(names, " "); got != want {
				t.Fatalf("kök listesi:\n got  %s\n want %s", got, want)
			}
			if l, err := s.fs.List("/dirlink/çok"); err != nil || len(l) != 300 {
				t.Fatalf("dirlink/çok: %d girdi, %v", len(l), err)
			}
			if err := s.fs.Mkdir("/x"); !errors.Is(err, ext2fs.ErrReadOnly) {
				t.Fatalf("salt-okunurda mkdir: %v", err)
			}
			s.close(t)

			// --- yazma ---
			s = open(t, enc, true)
			if info, _ := s.fs.Info(); info.ReadOnly {
				t.Fatalf("yazılabilir açılmadı: %s", info.ROReason)
			}
			big := randBytes(5_555_555)
			mt := time.Date(2031, 5, 6, 7, 8, 9, 0, time.UTC)
			must(t, s.fs.Mkdir("/yeni dizin"))
			must(t, s.fs.Mkdir("/yeni dizin/iç"))
			mustN(t)(s.fs.Create("/yeni dizin/büyük.bin", bytes.NewReader(big), int64(len(big)), false, mt))
			mustN(t)(s.fs.Create("/küçük.txt", strings.NewReader("selam"), 5, false, time.Time{}))
			mustN(t)(s.fs.Create("/bos.txt", strings.NewReader(""), 0, false, time.Time{}))
			if _, err := s.fs.Create("/küçük.txt", strings.NewReader("x"), 1, false, time.Time{}); !errors.Is(err, ext2fs.ErrExists) {
				t.Fatalf("var olan dosya: %v", err)
			}
			mustN(t)(s.fs.Create("/a.txt", strings.NewReader("üzerine yazıldı"), 0, true, time.Time{}))
			// htree dizinine ekle
			for i := 0; i < 200; i++ {
				mustN(t)(s.fs.Create(fmt.Sprintf("/alt/çok/yeni%03d", i), strings.NewReader("y"), 1, false, time.Time{}))
			}
			// silme
			must(t, s.fs.Remove("/alt/rand.bin", false))
			if err := s.fs.Remove("/alt", false); !errors.Is(err, ext2fs.ErrNotEmpty) {
				t.Fatalf("dolu dizin silme: %v", err)
			}
			must(t, s.fs.Remove("/link", false))
			for i := 0; i < 100; i++ {
				must(t, s.fs.Remove(fmt.Sprintf("/alt/çok/f%03d.txt", i), false))
			}
			// taşıma
			must(t, s.fs.Rename("/küçük.txt", "/yeni dizin/iç/taşındı.txt"))
			must(t, s.fs.Mkdir("/taşınacak"))
			must(t, s.fs.Mkdir("/taşınacak/alt"))
			must(t, s.fs.Rename("/taşınacak", "/yeni dizin/iç/taşındı-dizin"))
			if err := s.fs.Rename("/yeni dizin", "/yeni dizin/iç/döngü"); err == nil {
				t.Fatal("dizin kendi içine taşındı")
			}
			// özyinelemeli silme
			must(t, s.fs.Mkdir("/sil"))
			must(t, s.fs.Mkdir("/sil/a"))
			mustN(t)(s.fs.Create("/sil/a/f", bytes.NewReader(randBytes(100_000)), 0, false, time.Time{}))
			must(t, s.fs.Remove("/sil", true))
			// aynı oturumda geri oku
			if got := readAll(t, s.fs, "/yeni dizin/büyük.bin"); !bytes.Equal(got, big) {
				t.Fatal("büyük dosya geri okunamadı")
			}
			if e, err := s.fs.Stat("/yeni dizin/büyük.bin"); err != nil || !e.Mtime.Equal(mt) {
				t.Fatalf("mtime %v %v", e.Mtime, err)
			}
			s.close(t)

			// --- bağımsız doğrulama ---
			dec := testimg.Decrypt(t, enc)
			testimg.Fsck(t, dec)
			// debugfs cat inline_data dosyalarında içeriği blok sonuna kadar
			// sıfırla doldurarak basıyor (kendi hatası); boyutu stat'tan al.
			check := func(p string, want []byte) {
				t.Helper()
				st := string(debugfs(t, dec, "stat \""+p+"\""))
				m := sizeRe.FindStringSubmatch(st)
				if m == nil || m[1] != fmt.Sprint(len(want)) {
					t.Fatalf("debugfs %s: boyut farklı (istenen %d)\n%s", p, len(want), st)
				}
				got := debugfs(t, dec, "cat \""+p+"\"")
				if len(got) < len(want) || !bytes.Equal(got[:len(want)], want) {
					t.Fatalf("debugfs %s: içerik farklı", p)
				}
			}
			check("/yeni dizin/büyük.bin", big)
			check("/yeni dizin/iç/taşındı.txt", []byte("selam"))
			check("/a.txt", []byte("üzerine yazıldı"))
			check("/alt/çok/yeni199", []byte("y"))
			check("/alt/çok/f299.txt", []byte("299"))
			ls := string(debugfs(t, dec, "ls -l /"))
			for _, gone := range []string{"küçük.txt", " link", "sil", "taşınacak", ".luksweb"} {
				if strings.Contains(ls, gone) {
					t.Fatalf("kökte %q kalmış:\n%s", gone, ls)
				}
			}
			if st := string(debugfs(t, dec, "stat \"/yeni dizin/büyük.bin\"")); !strings.Contains(st, "2031") {
				t.Fatalf("mtime debugfs'te yok:\n%s", st)
			}

			// Kapatınca süper blok yeniden temiz olmalı, tekrar yazılabilir açılmalı.
			s = open(t, enc, true)
			if info, _ := s.fs.Info(); info.ReadOnly {
				t.Fatalf("ikinci açılış salt-okunur: %s", info.ROReason)
			}
			s.close(t)
		})
	}
}

// İşlem bittikten sonra disk boşta: fs.Close olmadan çekilse bile temiz olmalı.
func TestIdleIsClean(t *testing.T) {
	testimg.Need(t, "e2fsck")
	_, enc := testimg.Make(t, t.TempDir(), testimg.Options{SizeMB: 32})
	s := open(t, enc, true)
	must(t, s.fs.Mkdir("/d"))
	mustN(t)(s.fs.Create("/d/f", strings.NewReader("veri"), 0, false, time.Time{}))
	s.dev.Close() // fs.Close çağrılmadan "diski çekti"

	testimg.Fsck(t, testimg.Decrypt(t, enc))
	s2 := open(t, enc, true)
	if info, _ := s2.fs.Info(); info.ReadOnly {
		t.Fatalf("boştayken çekilen disk salt-okunur açıldı: %s", info.ROReason)
	}
	if got := readAll(t, s2.fs, "/d/f"); string(got) != "veri" {
		t.Fatalf("içerik %q", got)
	}
	s2.close(t)
}

// unplugReader ilk parçadan sonra aygıtı kapatır (yükleme ortasında disk çekildi).
type unplugReader struct {
	n      int
	unplug func()
}

func (r *unplugReader) Read(p []byte) (int, error) {
	r.n++
	if r.n == 2 {
		r.unplug()
	}
	if r.n > 3 {
		return 0, io.EOF
	}
	n := min(len(p), 1<<20)
	clear(p[:n])
	return n, nil
}

// Yükleme ortasında kesilen oturumdan sonra yazma kapatılmalı.
func TestCrashMidUploadBecomesReadOnly(t *testing.T) {
	testimg.Need(t, "e2fsck")
	_, enc := testimg.Make(t, t.TempDir(), testimg.Options{SizeMB: 32})
	s := open(t, enc, true)
	must(t, s.fs.Mkdir("/d"))
	_, err := s.fs.Create("/d/yarim", &unplugReader{unplug: func() { s.dev.Close() }}, 0, false, time.Time{})
	if err == nil {
		t.Fatal("aygıt kapandığı halde yükleme başarılı")
	}
	s.fs.Close()

	s2 := open(t, enc, true)
	info, _ := s2.fs.Info()
	if !info.ReadOnly || !strings.Contains(info.ROReason, "temiz") {
		t.Fatalf("yarım kalan FS yazılabilir açıldı: %+v", info)
	}
	if _, err := s2.fs.List("/d"); err != nil {
		t.Fatal(err)
	}
	s2.fs.Close()
	s2.dev.Close()
}

// blockingReader bir parça verir, sonra unblock kapanana dek bekler.
type blockingReader struct {
	n       int
	unblock chan struct{}
	started chan struct{}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	r.n++
	if r.n == 2 {
		close(r.started)
		<-r.unblock
	}
	n := min(len(p), 1<<20)
	clear(p[:n])
	return n, nil
}

// Close süren yüklemenin iptal edilip temizlenmesini beklemeli.
func TestCloseWaitsForUpload(t *testing.T) {
	testimg.Need(t, "e2fsck", "debugfs")
	_, enc := testimg.Make(t, t.TempDir(), testimg.Options{SizeMB: 32})
	s := open(t, enc, true)
	r := &blockingReader{unblock: make(chan struct{}), started: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := s.fs.Create("/akan", r, 0, false, time.Time{})
		done <- err
	}()
	<-r.started
	closed := make(chan error, 1)
	go func() { closed <- s.fs.Close() }()
	time.Sleep(300 * time.Millisecond)
	select {
	case <-closed:
		t.Fatal("Close yüklemeyi beklemedi")
	default:
	}
	close(r.unblock)
	if err := <-done; !errors.Is(err, ext2fs.ErrClosed) {
		t.Fatalf("yükleme hatası: %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	s.dev.Close()
	dec := testimg.Decrypt(t, enc)
	testimg.Fsck(t, dec)
	if ls := string(debugfs(t, dec, "ls /")); strings.Contains(ls, "akan") {
		t.Fatalf("yarım dosya kalmış:\n%s", ls)
	}
}

func TestNoSpace(t *testing.T) {
	testimg.Need(t, "e2fsck", "debugfs")
	_, enc := testimg.Make(t, t.TempDir(), testimg.Options{SizeMB: 32})
	s := open(t, enc, true)
	_, err := s.fs.Create("/dev.bin", bytes.NewReader(make([]byte, 64<<20)), 0, false, time.Time{})
	if !errors.Is(err, ext2fs.ErrNoSpace) {
		t.Fatalf("beklenen ErrNoSpace, gelen %v", err)
	}
	if _, err := s.fs.Stat("/dev.bin"); !errors.Is(err, ext2fs.ErrNotFound) {
		t.Fatalf("yarım dosya kalmış: %v", err)
	}
	s.close(t)
	testimg.Fsck(t, testimg.Decrypt(t, enc))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustN(t *testing.T) func(int64, error) {
	return func(_ int64, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
}
