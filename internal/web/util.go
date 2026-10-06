package web

import (
	"archive/zip"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"

	"luks_web/internal/ext2fs"
)

func cleanPath(p string) string { return path.Clean("/" + p) }

func parentDir(p string) string { return path.Dir(cleanPath(p)) }

// contentDisposition RFC 6266/5987: ASCII yedek ad + UTF-8 ad.
func contentDisposition(name string, inline bool) string {
	kind := "attachment"
	if inline {
		kind = "inline"
	}
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, kind, ascii, url.PathEscape(name))
}

// zip bir dizini sıkıştırmadan (store) zip olarak akıtır. Sembolik bağlar izlenmez.
func (s *Server) zip(w http.ResponseWriter, r *http.Request) {
	f := s.fs(w)
	if f == nil {
		return
	}
	root := cleanPath(r.URL.Query().Get("path"))
	st, err := f.Stat(root)
	if err != nil {
		fail(w, err)
		return
	}
	if st.Type != "dir" && st.Type != "dirlink" {
		fail(w, ext2fs.ErrNotDir)
		return
	}
	name := path.Base(root)
	if root == "/" {
		name = "kok"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(name+".zip", false))
	zw := zip.NewWriter(w)
	if err := zipDir(f, zw, root, name, 0); err != nil {
		// Başlıklar gitti; yarım zip'i bozuk bırakmak en dürüstü.
		log.Printf("zip %s: %v", root, err)
		return
	}
	zw.Close()
}

func zipDir(f *ext2fs.FS, zw *zip.Writer, dir, prefix string, depth int) error {
	if depth > 256 {
		return fmt.Errorf("dizin çok derin")
	}
	ents, err := f.List(dir)
	if err != nil {
		return err
	}
	if _, err := zw.CreateHeader(&zip.FileHeader{Name: prefix + "/", Method: zip.Store}); err != nil {
		return err
	}
	for _, e := range ents {
		p := path.Join(dir, e.Name)
		zp := prefix + "/" + e.Name
		switch e.Type {
		case "dir":
			if err := zipDir(f, zw, p, zp, depth+1); err != nil {
				return err
			}
		case "file":
			hdr := &zip.FileHeader{Name: zp, Method: zip.Store, Modified: e.Mtime}
			hdr.SetMode(0o644)
			out, err := zw.CreateHeader(hdr)
			if err != nil {
				return err
			}
			in, err := f.OpenFile(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, in)
			in.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}
