// luks-web: LUKS2 + ext4 şifreli diske tarayıcıdan erişim.
//
// Kullanım:
//
//	luks-web [-port N] [-no-browser] [imaj-veya-aygıt ...]
//
// Yol verilmezse Windows'ta tüm fiziksel diskler (\\.\PhysicalDriveN) taranır.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"luks_web/internal/web"
)

var version = "geliştirme"

func main() {
	port := flag.Int("port", 0, "dinlenecek port (0 = rastgele)")
	noBrowser := flag.Bool("no-browser", false, "tarayıcıyı otomatik açma")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "luks-web %s\n\nKullanım: %s [seçenekler] [imaj-veya-aygıt ...]\n\n", version, os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	log.SetFlags(log.Ltime)

	srv := web.NewServer(flag.Args())
	ln, url, err := srv.Listen(*port)
	if err != nil {
		fatal(err)
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	fmt.Printf("luks-web %s\n\nTarayıcıda açın:\n\n  %s\n\n", version, url)
	fmt.Println("Kapatmak için arayüzdeki \"Çık\" düğmesini kullanın ya da bu pencerede Ctrl+C'ye basın.")
	fmt.Println("(Disk açıkken pencereyi kapatmayın; önce Çık deyin.)")
	if !*noBrowser {
		openBrowser(url)
	}

	go func() {
		if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
			fatal(err)
		}
	}()

	sig := make(chan os.Signal, 1)
	// Windows'ta konsol penceresinin kapatılması da SIGTERM olarak gelir.
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Printf("sinyal: %v, kapatılıyor", s)
	case <-srv.Quit:
		log.Printf("arayüzden çıkış istendi")
	}

	// Süren istekler için kısa süre bekle, sonra bağlantıları kes: yarım
	// yüklemeler iptal olur ve fs.Close onları temizler.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := hs.Shutdown(ctx); err != nil {
		hs.Close()
	}
	cancel()
	if err := srv.Lock(); err != nil {
		log.Printf("UYARI: disk düzgün kapatılamadı: %v", err)
		pause()
		os.Exit(1)
	}
	log.Printf("güle güle")
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("tarayıcı açılamadı (%v); adresi elle açın", err)
		return
	}
	go cmd.Wait()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "HATA:", err)
	pause()
	os.Exit(1)
}

// pause Windows'ta çift tıklamayla açılan konsol hemen kapanmasın diye bekler.
func pause() {
	if runtime.GOOS == "windows" {
		fmt.Fprintln(os.Stderr, "\nKapatmak için Enter'a basın…")
		fmt.Scanln()
	}
}
