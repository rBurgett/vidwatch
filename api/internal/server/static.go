package server

import (
	"net/http"
	"path/filepath"
	"strings"
)

// pages maps pretty URLs to files. In production the CloudFront Function in
// infra/vidwatch.yaml does the same rewrite; this copy is for local dev. ("/"
// needs no entry: the file server already serves index.html for it.)
var pages = map[string]string{
	"/login":         "/login.html",
	"/register":      "/register.html",
	"/upload":        "/upload.html",
	"/watch":         "/watch.html",
	"/trending":      "/trending.html",
	"/library":       "/library.html",
	"/playlist":      "/playlist.html",
	"/settings":      "/settings.html",
	"/auth/callback": "/auth-callback.html",
}

func staticHandler(dir string) http.Handler {
	files := http.FileServer(http.Dir(filepath.Clean(dir)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if page, ok := pages[p]; ok {
			p = page
		} else if strings.HasPrefix(p, "/u/") && !strings.Contains(p, ".") {
			p = "/profile.html"
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = p
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r2)
	})
}
