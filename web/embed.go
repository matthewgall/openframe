// Package web embeds the built admin SPA (web/admin/dist) into the
// server binary and serves it.
//
// The SPA is built with Vite (web/admin). `make web` regenerates dist;
// the built assets are committed so that a plain `go build` works
// without Node (ADR-0005).
//
// Serving model:
//
//   - existing files are served with their MIME type
//   - extensionless paths (e.g. /library) fall back to index.html so the
//     SPA can own client-side routes in M2+
//   - anything else is a 404
package web

import (
	"bytes"
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"
)

//go:embed all:admin/dist
var dist embed.FS

// distData caches the built assets once at startup. The admin UI is
// small (< 1 MiB), so per-request open/read would be pure waste on a
// 1 GB frame-adjacent box.
var (
	distFiles = map[string][]byte{}
	distMIME  = map[string]string{}
	distIndex []byte
	dist404   []byte = []byte("404 not found\n")
)

func init() {
	sub, err := fs.Sub(dist, "admin/dist")
	if err != nil {
		panic(err)
	}
	_ = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		distFiles[p] = data
		mt := mime.TypeByExtension(filepath.Ext(p))
		if mt == "" {
			mt = "application/octet-stream"
		}
		distMIME[p] = mt
		return nil
	})
	if idx, ok := distFiles["index.html"]; ok {
		distIndex = idx
	}
}

// SPAHandler serves the admin SPA with client-side routing fallback.
//
// The router mounts this on a GET catch-all, so it must decline API
// paths: /api/* always gets a JSON 404 here, never the app shell.
func SPAHandler() http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if strings.HasPrefix(p, "api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"not_found","message":"no such api endpoint"}` + "\n"))
			return
		}
		if data, ok := distFiles[p]; ok {
			writeFile(w, r, p, data)
			return
		}
		// SPA fallback: extensionless paths get the app shell.
		if !strings.Contains(path.Base(p), ".") {
			writeFile(w, r, "index.html", distIndex)
			return
		}
		http.Error(w, "404 not found", http.StatusNotFound)
	})
}

// NotFoundHandler is the router-wide 404. API paths get a JSON 404;
// extensionless non-API paths fall back to the SPA shell (client-side
// routing); anything else is a plain 404.
func NotFoundHandler() http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"not_found","message":"no such api endpoint"}` + "\n"))
			return
		}
		if p != "" && !strings.Contains(path.Base(p), ".") {
			writeFile(w, r, "index.html", distIndex)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		w.Write(dist404)
	})
}

func writeFile(w http.ResponseWriter, r *http.Request, name string, data []byte) {
	w.Header().Set("Content-Type", distMIME[name])
	w.Header().Set("Cache-Control", "no-cache")
	// Assets are rebuilt by re-releasing the binary; no ETag dance needed.
	http.ServeContent(w, r, name, time.Unix(0, 0).UTC(), bytes.NewReader(data))
}
