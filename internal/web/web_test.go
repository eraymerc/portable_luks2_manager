package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"luks_web/internal/testimg"
)

type client struct {
	t    *testing.T
	base string
	c    *http.Client
}

func (c *client) do(method, path string, body io.Reader, hdr map[string]string) (*http.Response, []byte) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.c.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (c *client) post(path string, v any, want int) map[string]any {
	c.t.Helper()
	b, _ := json.Marshal(v)
	resp, body := c.do("POST", path, bytes.NewReader(b), map[string]string{"X-LuksWeb": "1", "Content-Type": "application/json"})
	if resp.StatusCode != want {
		c.t.Fatalf("POST %s: %d %s (beklenen %d)", path, resp.StatusCode, body, want)
	}
	var out map[string]any
	json.Unmarshal(body, &out)
	return out
}

func (c *client) get(path string, want int) []byte {
	c.t.Helper()
	resp, body := c.do("GET", path, nil, nil)
	if resp.StatusCode != want {
		c.t.Fatalf("GET %s: %d %s (beklenen %d)", path, resp.StatusCode, body, want)
	}
	return body
}

func TestEndToEnd(t *testing.T) {
	testimg.Need(t, "e2fsck")
	_, enc := testimg.Make(t, t.TempDir(), testimg.Options{SizeMB: 48})

	srv := NewServer([]string{enc})
	ln, link, err := srv.Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(ln)
	defer hs.Close()

	u, _ := url.Parse(link)
	base := "http://" + u.Host
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: base, c: &http.Client{Jar: jar}}

	// Belirteçsiz erişim yok.
	c.get("/api/state", 401)
	c.get("/?token=yanlis", 401)
	// Belirteç -> çerez -> yönlendirme.
	if b := c.get("/?token="+srv.Token, 200); !strings.Contains(string(b), "luks_web") {
		t.Fatal("ana sayfa gelmedi")
	}
	// Host denetimi (DNS rebinding).
	req, _ := http.NewRequest("GET", base+"/api/state", nil)
	req.Host = "evil.example:80"
	if resp, _ := c.c.Do(req); resp.StatusCode != 403 {
		t.Fatalf("yabancı Host kabul edildi: %d", resp.StatusCode)
	}
	// CSRF: özel başlıksız POST reddedilir.
	if resp, _ := c.do("POST", "/api/lock", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("başlıksız POST kabul edildi: %d", resp.StatusCode)
	}

	var devs struct {
		Devices []struct{ ID string } `json:"devices"`
	}
	json.Unmarshal(c.get("/api/devices", 200), &devs)
	if len(devs.Devices) != 1 {
		t.Fatalf("aygıt sayısı %d", len(devs.Devices))
	}
	id := devs.Devices[0].ID

	c.post("/api/unlock", map[string]any{"id": id, "password": "yanlış"}, 401)
	c.post("/api/unlock", map[string]any{"id": id, "password": testimg.Passphrase}, 200)

	// Yükle (eksik ara dizinleri oluşturarak), sonra indir.
	data := bytes.Repeat([]byte("0123456789abcdef"), 300_000) // 4.8 MB
	resp, body := c.do("POST", "/api/upload?"+url.Values{"path": {"/a/b/ç ğ.bin"}, "mkdirs": {"1"}}.Encode(),
		bytes.NewReader(data), map[string]string{"X-LuksWeb": "1"})
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}
	resp, body = c.do("POST", "/api/upload?"+url.Values{"path": {"/a/b/ç ğ.bin"}}.Encode(),
		bytes.NewReader(data), map[string]string{"X-LuksWeb": "1"})
	if resp.StatusCode != 409 {
		t.Fatalf("çakışan upload: %d %s", resp.StatusCode, body)
	}
	if got := c.get("/api/download?"+url.Values{"path": {"/a/b/ç ğ.bin"}}.Encode(), 200); !bytes.Equal(got, data) {
		t.Fatal("indirilen veri farklı")
	}
	// Range desteği
	resp, body = c.do("GET", "/api/download?"+url.Values{"path": {"/a/b/ç ğ.bin"}}.Encode(), nil,
		map[string]string{"Range": "bytes=1000000-1000015"})
	if resp.StatusCode != 206 || !bytes.Equal(body, data[1000000:1000016]) {
		t.Fatalf("range: %d %q", resp.StatusCode, body)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "UTF-8''%C3%A7%20%C4%9F.bin") {
		t.Fatalf("Content-Disposition: %s", cd)
	}

	// Zip
	zb := c.get("/api/zip?path=/a", 200)
	zr, err := zip.NewReader(bytes.NewReader(zb), int64(len(zb)))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range zr.File {
		if f.Name == "a/b/ç ğ.bin" {
			rc, _ := f.Open()
			got, _ := io.ReadAll(rc)
			found = bytes.Equal(got, data)
		}
	}
	if !found {
		t.Fatal("zip içinde dosya yok/bozuk")
	}

	c.post("/api/mkdir", map[string]any{"path": "/yeni"}, 200)
	c.post("/api/mkdir", map[string]any{"path": "/yeni"}, 409)
	c.post("/api/rename", map[string]any{"from": "/yeni", "to": "/a/yeni2"}, 200)
	c.post("/api/delete", map[string]any{"path": "/a"}, 409) // boş değil
	var list struct {
		Entries []struct{ Name, Type string } `json:"entries"`
	}
	json.Unmarshal(c.get("/api/list?path=/a", 200), &list)
	if len(list.Entries) != 2 {
		t.Fatalf("liste: %+v", list.Entries)
	}
	c.post("/api/delete", map[string]any{"path": "/a", "recursive": true}, 200)
	c.get("/api/list?path=/a", 404)

	// Kalıcı bir dosya bırak, çık ve bağımsız doğrula.
	resp, _ = c.do("POST", "/api/upload?path=/kalici.txt", strings.NewReader("kalıcı"), map[string]string{"X-LuksWeb": "1"})
	if resp.StatusCode != 200 {
		t.Fatal("upload kalici")
	}
	c.post("/api/quit", nil, 200)
	select {
	case <-srv.Quit:
	default:
		t.Fatal("Quit kanalı kapanmadı")
	}
	c.get("/api/list?path=/", 409) // kilitli

	dec := testimg.Decrypt(t, enc)
	testimg.Fsck(t, dec)
}
