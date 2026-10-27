package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
)

func init() {
	mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// staticVersion is a hash of all embedded static files. It goes into asset
// URLs (?v=…), so every build that changes CSS/JS/icons gets new URLs and
// browsers and Cloudflare fetch the new files instead of stale cached ones.
var staticVersion = func() string {
	h := sha256.New()
	fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(path)
		if err != nil {
			return err
		}
		h.Write([]byte(path))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:10]
}()

// asset returns the versioned URL of a file in static/.
func asset(name string) string {
	return "/static/" + name + "?v=" + staticVersion
}

// staticHandler serves static/. Versioned URLs are cached for a year;
// unversioned ones (like the manifest) must be revalidated.
func staticHandler() http.Handler {
	sub, _ := fs.Sub(staticFS, "static")
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") == staticVersion {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// Browsers request /favicon.ico on their own; answer with the PNG icon.
func serveFavicon(w http.ResponseWriter, r *http.Request) {
	b, _ := staticFS.ReadFile("static/favicon.png")
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(b)
}
