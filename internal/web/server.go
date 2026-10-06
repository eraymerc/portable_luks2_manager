// Package web tarayıcı arayüzünü ve JSON API'sini sunar. Yalnızca
// 127.0.0.1'e bağlanır; her istek rastgele bir oturum belirteci ister.
package web

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"luks_web/internal/blockdev"
	"luks_web/internal/ext2fs"
	"luks_web/internal/luks2"
)

//go:embed static
var staticFS embed.FS

const cookieName = "luksweb"

// Server uygulama durumudur. Aynı anda tek disk açık olabilir.
type Server struct {
	Token string
	Paths []string // taranacak yollar; boşsa (Windows'ta) fiziksel diskler
	Quit  chan struct{}

	addr string

	mu    sync.Mutex
	cands []blockdev.Candidate
	sess  *session
	quit  sync.Once
}

type session struct {
	cand blockdev.Candidate
	dev  blockdev.Device
	fs   *ext2fs.FS
	luks string // LUKS UUID
}

func NewServer(paths []string) *Server {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return &Server{Token: hex.EncodeToString(b), Paths: paths, Quit: make(chan struct{})}
}

// Listen 127.0.0.1 üzerinde dinlemeye başlar ve tarayıcı adresini döndürür.
func (s *Server) Listen(port int) (net.Listener, string, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, "", err
	}
	s.addr = ln.Addr().String()
	return ln, fmt.Sprintf("http://%s/?token=%s", s.addr, s.Token), nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(static)

	mux.HandleFunc("GET /{$}", s.index)
	mux.Handle("GET /static/", http.StripPrefix("/static/", files))
	mux.HandleFunc("GET /api/devices", s.devices)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/unlock", s.unlock)
	mux.HandleFunc("POST /api/lock", s.lockHandler)
	mux.HandleFunc("POST /api/quit", s.quitHandler)
	mux.HandleFunc("GET /api/list", s.list)
	mux.HandleFunc("GET /api/download", s.download)
	mux.HandleFunc("GET /api/zip", s.zip)
	mux.HandleFunc("POST /api/upload", s.upload)
	mux.HandleFunc("POST /api/mkdir", s.mkdir)
	mux.HandleFunc("POST /api/delete", s.remove)
	mux.HandleFunc("POST /api/rename", s.rename)
	return s.guard(mux)
}

// guard: Host denetimi (DNS rebinding), belirteç, CSRF ve güvenlik başlıkları.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		_, port, _ := net.SplitHostPort(s.addr)
		if host != s.addr && host != "localhost:"+port {
			http.Error(w, "geçersiz Host", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")

		// İlk açılış: ?token=... -> çerez, sonra adresten belirteci sil.
		if t := r.URL.Query().Get("token"); t != "" && r.URL.Path == "/" {
			if subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) == 1 {
				http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.Token, Path: "/",
					HttpOnly: true, SameSite: http.SameSiteStrictMode})
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
		}
		c, err := r.Cookie(cookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.Token)) != 1 {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				jsonErr(w, http.StatusUnauthorized, errors.New("oturum yok; konsoldaki bağlantıyı açın"))
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "Bu sayfayı luks-web konsolunda yazan bağlantıyla (token içeren) açın.\n")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Özel başlık başka kökenden gelen formlarla gönderilemez (preflight gerekir).
			if r.Header.Get("X-LuksWeb") != "1" {
				jsonErr(w, http.StatusForbidden, errors.New("CSRF denetimi"))
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+host {
				jsonErr(w, http.StatusForbidden, errors.New("geçersiz köken"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

// ---------------- yardımcılar ----------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, ext2fs.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ext2fs.ErrExists), errors.Is(err, ext2fs.ErrNotEmpty), errors.Is(err, ext2fs.ErrBusy):
		return http.StatusConflict
	case errors.Is(err, ext2fs.ErrReadOnly):
		return http.StatusForbidden
	case errors.Is(err, ext2fs.ErrNoSpace):
		return http.StatusInsufficientStorage
	case errors.Is(err, ext2fs.ErrBadName), errors.Is(err, ext2fs.ErrNotDir), errors.Is(err, ext2fs.ErrIsDir):
		return http.StatusBadRequest
	case errors.Is(err, luks2.ErrWrongPassphrase):
		return http.StatusUnauthorized
	}
	return http.StatusInternalServerError
}

func fail(w http.ResponseWriter, err error) { jsonErr(w, statusFor(err), err) }

func readBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// fs açık oturumun dosya sistemini döndürür.
func (s *Server) fs(w http.ResponseWriter) *ext2fs.FS {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess == nil {
		jsonErr(w, http.StatusConflict, errors.New("disk kilitli"))
		return nil
	}
	return s.sess.fs
}

// ---------------- disk / kilit ----------------

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	cands, errs := blockdev.ScanAll(s.Paths)
	s.mu.Lock()
	s.cands = cands
	s.mu.Unlock()
	var es []string
	for _, e := range errs {
		es = append(es, e.Error())
	}
	if cands == nil {
		cands = []blockdev.Candidate{}
	}
	writeJSON(w, map[string]any{"devices": cands, "errors": es})
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	if sess == nil {
		writeJSON(w, map[string]any{"unlocked": false})
		return
	}
	info, err := sess.fs.Info()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"unlocked": true, "device": sess.cand, "luksUUID": sess.luks, "fs": info})
}

func (s *Server) unlock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Password string `json:"password"`
		ReadOnly bool   `json:"readOnly"`
	}
	if err := readBody(r, &req); err != nil {
		jsonErr(w, 400, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess != nil {
		jsonErr(w, http.StatusConflict, errors.New("zaten açık bir disk var; önce kilitleyin"))
		return
	}
	var cand *blockdev.Candidate
	for i := range s.cands {
		if s.cands[i].ID == req.ID {
			cand = &s.cands[i]
		}
	}
	if cand == nil {
		jsonErr(w, 404, errors.New("disk bulunamadı; listeyi yenileyin"))
		return
	}
	sess, err := openSession(*cand, []byte(req.Password), !req.ReadOnly)
	if err != nil {
		log.Printf("açma başarısız: %v", err)
		fail(w, err)
		return
	}
	s.sess = sess
	info, _ := sess.fs.Info()
	log.Printf("açıldı: %s (%s)", cand, map[bool]string{true: "salt-okunur: " + info.ROReason, false: "yazılabilir"}[info.ReadOnly])
	writeJSON(w, map[string]any{"ok": true})
}

func openSession(c blockdev.Candidate, pass []byte, writable bool) (*session, error) {
	defer clear(pass)
	dev, err := blockdev.OpenCandidate(c, writable)
	if err != nil && writable {
		// Yazma erişimi yoksa (ör. yönetici değil) salt-okunur dene.
		dev, err = blockdev.OpenCandidate(c, false)
		writable = false
	}
	if err != nil {
		return nil, err
	}
	hdr, err := luks2.ReadHeader(dev)
	if err != nil {
		dev.Close()
		return nil, err
	}
	mk, err := hdr.Unlock(dev, pass)
	if err != nil {
		dev.Close()
		return nil, err
	}
	vol, err := hdr.OpenVolume(dev, mk)
	clear(mk)
	if err != nil {
		dev.Close()
		return nil, err
	}
	fsys, err := ext2fs.Open(vol, writable)
	if err != nil {
		dev.Close()
		return nil, err
	}
	return &session{cand: c, dev: dev, fs: fsys, luks: hdr.UUID}, nil
}

// Lock açık diski kapatır (süper blok temiz işaretlenir).
func (s *Server) Lock() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess == nil {
		return nil
	}
	err := s.sess.fs.Close()
	if cerr := s.sess.dev.Close(); err == nil {
		err = cerr
	}
	log.Printf("kilitlendi: %s", s.sess.cand)
	s.sess = nil
	return err
}

func (s *Server) lockHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.Lock(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) quitHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.Lock(); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
	s.quit.Do(func() { close(s.Quit) })
}

// ---------------- dosya işlemleri ----------------

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	f := s.fs(w)
	if f == nil {
		return
	}
	p := r.URL.Query().Get("path")
	ents, err := f.List(p)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"path": cleanPath(p), "entries": ents})
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	f := s.fs(w)
	if f == nil {
		return
	}
	p := r.URL.Query().Get("path")
	st, err := f.Stat(p)
	if err != nil {
		fail(w, err)
		return
	}
	file, err := f.OpenFile(p)
	if err != nil {
		fail(w, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Disposition", contentDisposition(st.Name, r.URL.Query().Get("inline") == "1"))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", st.Mtime, file)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	f := s.fs(w)
	if f == nil {
		return
	}
	q := r.URL.Query()
	p := cleanPath(q.Get("path"))
	if q.Get("mkdirs") == "1" {
		if err := mkdirAll(f, parentDir(p)); err != nil {
			fail(w, err)
			return
		}
	}
	var mtime time.Time
	if ms := q.Get("mtime"); ms != "" {
		var v int64
		if _, err := fmt.Sscan(ms, &v); err == nil && v > 0 {
			mtime = time.UnixMilli(v)
		}
	}
	n, err := f.Create(p, r.Body, r.ContentLength, q.Get("overwrite") == "1", mtime)
	if err != nil {
		log.Printf("yükleme başarısız %s: %v", p, err)
		fail(w, err)
		return
	}
	log.Printf("yüklendi: %s (%d bayt)", p, n)
	writeJSON(w, map[string]any{"ok": true, "size": n})
}

func mkdirAll(f *ext2fs.FS, dir string) error {
	if dir == "/" {
		return nil
	}
	if st, err := f.Stat(dir); err == nil {
		if st.Type != "dir" && st.Type != "dirlink" {
			return fmt.Errorf("%s: %w", dir, ext2fs.ErrNotDir)
		}
		return nil
	} else if !errors.Is(err, ext2fs.ErrNotFound) {
		return err
	}
	if err := mkdirAll(f, parentDir(dir)); err != nil {
		return err
	}
	if err := f.Mkdir(dir); err != nil && !errors.Is(err, ext2fs.ErrExists) {
		return err
	}
	return nil
}

func (s *Server) mkdir(w http.ResponseWriter, r *http.Request) {
	f := s.fs(w)
	if f == nil {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := readBody(r, &req); err != nil {
		jsonErr(w, 400, err)
		return
	}
	if err := f.Mkdir(req.Path); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	f := s.fs(w)
	if f == nil {
		return
	}
	var req struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := readBody(r, &req); err != nil {
		jsonErr(w, 400, err)
		return
	}
	if err := f.Remove(req.Path, req.Recursive); err != nil {
		fail(w, err)
		return
	}
	log.Printf("silindi: %s", cleanPath(req.Path))
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) rename(w http.ResponseWriter, r *http.Request) {
	f := s.fs(w)
	if f == nil {
		return
	}
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := readBody(r, &req); err != nil {
		jsonErr(w, 400, err)
		return
	}
	if err := f.Rename(req.From, req.To); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
