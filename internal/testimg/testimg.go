// Package testimg testler için cryptsetup + mkfs.ext4 ile root gerektirmeden
// LUKS2/ext4 imajları üretir. Hakem (oracle) olarak dış araçlar kullanılır:
//   - Şifreleme: cryptsetup reencrypt --encrypt (çevrimdışı, kullanıcı alanında)
//   - Çözme:     cryptsetup reencrypt --decrypt  -> düz imaj -> e2fsck -fn / debugfs
package testimg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

const Passphrase = "test parola ğüşıöç"

// HeaderReserve imaj başına ayrılan LUKS2 başlık alanı (reduce-device-size).
const HeaderReserve = 16 << 20

type Options struct {
	SizeMB     int    // ext4 boyutu (MiB)
	SectorSize int    // LUKS sektör boyutu (varsayılan 4096)
	PBKDF      string // argon2id (varsayılan), argon2i, pbkdf2
	BlockSize  int    // ext4 blok boyutu (varsayılan 4096)
	Populate   string // mkfs.ext4 -d dizini (isteğe bağlı)
	MkfsArgs   []string
}

func Need(t testing.TB, tools ...string) {
	t.Helper()
	for _, tool := range append([]string{"cryptsetup", "mkfs.ext4"}, tools...) {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s bulunamadı", tool)
		}
	}
}

func run(t testing.TB, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out.String())
	}
}

// Make dir içinde plain.img (ext4) ve enc.img (aynı verinin LUKS2 hali) üretir.
func Make(t testing.TB, dir string, o Options) (plain, enc string) {
	t.Helper()
	if o.SizeMB == 0 {
		o.SizeMB = 64
	}
	if o.SectorSize == 0 {
		o.SectorSize = 4096
	}
	if o.PBKDF == "" {
		o.PBKDF = "argon2id"
	}
	if o.BlockSize == 0 {
		o.BlockSize = 4096
	}
	plain = filepath.Join(dir, "plain.img")
	enc = filepath.Join(dir, "enc.img")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(plain, int64(o.SizeMB)<<20+HeaderReserve); err != nil {
		t.Fatal(err)
	}
	args := []string{"-q", "-F", "-b", fmt.Sprint(o.BlockSize), "-L", "TESTFS"}
	args = append(args, o.MkfsArgs...)
	if o.Populate != "" {
		args = append(args, "-d", o.Populate)
	}
	args = append(args, plain, fmt.Sprintf("%dM", o.SizeMB))
	run(t, "mkfs.ext4", args...)

	data, err := os.ReadFile(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enc, data, 0o600); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte(Passphrase), 0o600); err != nil {
		t.Fatal(err)
	}
	eargs := []string{"reencrypt", "--disable-locks", "--encrypt", "--type", "luks2",
		"--reduce-device-size", fmt.Sprintf("%d", HeaderReserve/512) + "s",
		"--sector-size", fmt.Sprint(o.SectorSize), "--pbkdf", o.PBKDF,
		"--key-file", key, "--batch-mode"}
	if o.PBKDF == "pbkdf2" {
		eargs = append(eargs, "--pbkdf-force-iterations", "1000")
	} else {
		eargs = append(eargs, "--pbkdf-memory", "32768", "--pbkdf-force-iterations", "4")
	}
	run(t, "cryptsetup", append(eargs, enc)...)
	return plain, enc
}

// Decrypt enc imajını bağımsız olarak (yalnız cryptsetup ile) çözer ve düz
// ext4 imajının yolunu döndürür. cryptsetup gömülü başlıklı aygıtı yerinde
// çözmediği için: ana anahtar dışa alınır, veri alanı offset 0'lı ayrık bir
// başlıkla yeniden sarılır, sonra o çözülür.
func Decrypt(t testing.TB, enc string) string {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte(Passphrase), 0o600); err != nil {
		t.Fatal(err)
	}
	js, err := exec.Command("cryptsetup", "luksDump", "--disable-locks", "--dump-json-metadata", enc).Output()
	if err != nil {
		t.Fatalf("luksDump: %v", err)
	}
	var meta struct {
		Segments map[string]struct {
			Offset     string `json:"offset"`
			Encryption string `json:"encryption"`
			SectorSize int    `json:"sector_size"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(js, &meta); err != nil {
		t.Fatal(err)
	}
	seg := meta.Segments["0"]
	off, _ := strconv.ParseInt(seg.Offset, 10, 64)

	mk := filepath.Join(dir, "mk")
	run(t, "cryptsetup", "luksDump", "--disable-locks", "--dump-volume-key", "--volume-key-file", mk,
		"--key-file", key, "--batch-mode", enc)
	mkData, err := os.ReadFile(mk)
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(enc)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "plain.img")
	if err := os.WriteFile(out, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	hdr := filepath.Join(dir, "hdr")
	if err := os.WriteFile(hdr, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(hdr, 16<<20); err != nil {
		t.Fatal(err)
	}
	run(t, "cryptsetup", "luksFormat", "--disable-locks", "--type", "luks2", "--header", hdr,
		"--offset", "0", "--cipher", seg.Encryption, "--key-size", fmt.Sprint(len(mkData)*8),
		"--sector-size", fmt.Sprint(seg.SectorSize), "--volume-key-file", mk,
		"--key-file", key, "--pbkdf", "pbkdf2", "--pbkdf-force-iterations", "1000",
		"--batch-mode", out)
	run(t, "cryptsetup", "reencrypt", "--disable-locks", "--force-offline-reencrypt", "--decrypt",
		"--header", hdr, "--key-file", key, "--batch-mode", out)
	return out
}

// Fsck e2fsck -fn çalıştırır; temiz değilse testi düşürür.
func Fsck(t testing.TB, img string) {
	t.Helper()
	cmd := exec.Command("e2fsck", "-fn", img)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("e2fsck temiz değil: %v\n%s", err, out)
	}
}
