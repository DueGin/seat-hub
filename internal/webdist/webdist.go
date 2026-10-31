// Package webdist serves the built SPA. `make web` copies web/dist here before
// `go build`, so the released binary is self-contained. During development run
// the Vite dev server (web/ on :5173, proxying /api and /v1) instead.
package webdist

import (
	"embed"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// Handler serves the SPA: existing files as-is, everything else index.html.
func Handler(dir string) http.Handler {
	var src fs.FS
	if dir != "" {
		src = os.DirFS(dir)
	} else {
		sub, err := fs.Sub(embedded, "dist")
		if err != nil {
			src = nil
		} else {
			src = sub
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if src == nil {
			http.Error(w, "web build missing: run `pnpm -C web build` and copy web/dist", http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" || name == "." {
			name = "index.html"
		}
		if f, err := src.Open(name); err == nil {
			defer f.Close()
			if st, err := f.Stat(); err == nil && !st.IsDir() {
				setCacheHeaders(w, name)
				http.ServeContent(w, r, name, st.ModTime(), f.(ioReadSeeker))
				return
			}
		}
		// SPA fallback.
		index, err := fs.ReadFile(src, "index.html")
		if err != nil {
			http.Error(w, "index.html not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(index)
	})
}

func setCacheHeaders(w http.ResponseWriter, name string) {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".js", ".css", ".woff2", ".woff", ".ttf", ".png", ".svg", ".webp", ".jpg", ".ico":
		if strings.Contains(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
	default:
		w.Header().Set("Cache-Control", "no-cache")
	}
}

// ioReadSeeker is the small interface http.ServeContent needs; embed.FS files
// and os.File both satisfy it.
type ioReadSeeker interface {
	Read([]byte) (int, error)
	Seek(int64, int) (int64, error)
}
