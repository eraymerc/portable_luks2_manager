package luks2_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"testing"

	"luks_web/internal/blockdev"
	"luks_web/internal/luks2"
	"luks_web/internal/testimg"
)

// Çözülen verinin, cryptsetup'a verilen düz imajla birebir aynı olduğunu doğrular.
func TestDecryptMatchesPlain(t *testing.T) {
	testimg.Need(t)
	for _, tc := range []struct {
		pbkdf string
		ss    int
	}{
		{"argon2id", 4096}, {"argon2i", 4096}, {"pbkdf2", 4096}, {"argon2id", 512},
	} {
		t.Run(fmt.Sprintf("%s-%d", tc.pbkdf, tc.ss), func(t *testing.T) {
			dir := t.TempDir()
			plain, enc := testimg.Make(t, dir, testimg.Options{SizeMB: 16, SectorSize: tc.ss, PBKDF: tc.pbkdf})
			dev, err := blockdev.Open(enc, false)
			if err != nil {
				t.Fatal(err)
			}
			defer dev.Close()
			hdr, err := luks2.ReadHeader(dev)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := hdr.Unlock(dev, []byte("yanlış")); !errors.Is(err, luks2.ErrWrongPassphrase) {
				t.Fatalf("yanlış parola: beklenen ErrWrongPassphrase, gelen %v", err)
			}
			mk, err := hdr.Unlock(dev, []byte(testimg.Passphrase))
			if err != nil {
				t.Fatal(err)
			}
			vol, err := hdr.OpenVolume(dev, mk)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(plain)
			if err != nil {
				t.Fatal(err)
			}
			want = want[:16<<20]
			got := make([]byte, len(want))
			if _, err := vol.ReadAt(got, 0); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("çözülen veri düz imajla eşleşmiyor")
			}
		})
	}
}

// Kendi yazdığımızı cryptsetup'ın çözebildiğini doğrular.
func TestWriteRoundTrip(t *testing.T) {
	testimg.Need(t)
	dir := t.TempDir()
	_, enc := testimg.Make(t, dir, testimg.Options{SizeMB: 16})
	dev, err := blockdev.Open(enc, true)
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
	pat := bytes.Repeat([]byte("luks_web yazma testi "), 4096)[:3*4096*7]
	const off = 5 << 20
	if _, err := vol.WriteAt(pat, off); err != nil {
		t.Fatal(err)
	}
	dev.Close()

	dec := testimg.Decrypt(t, enc)
	f, err := os.Open(dec)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := make([]byte, len(pat))
	if _, err := f.ReadAt(got, off); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pat) {
		t.Fatal("cryptsetup'ın çözdüğü veri yazdığımızla eşleşmiyor")
	}
}
