package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed index.html styles.css dist/app.js
var files embed.FS

func Handler() http.Handler {
	assets, err := fs.Sub(files, ".")
	if err != nil {
		panic("open embedded Manifold UI: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(assets))
	index, err := files.ReadFile("index.html")
	if err != nil {
		panic("read embedded Manifold UI: " + err.Error())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.Redirect(w, r, "/app/", http.StatusTemporaryRedirect)
		case "/assets/styles.css":
			serveAsset(fileServer, w, r, "/styles.css")
		case "/assets/app.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			serveAsset(fileServer, w, r, "/dist/app.js")
		default:
			if isAppRoute(r.URL.Path) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(index)
				return
			}
			http.NotFound(w, r)
		}
	})
}

func serveAsset(fileServer http.Handler, w http.ResponseWriter, r *http.Request, path string) {
	request := r.Clone(r.Context())
	request.URL.Path = path
	fileServer.ServeHTTP(w, request)
}

func isAppRoute(path string) bool {
	switch path {
	case "/app", "/app/", "/login", "/explorer", "/search", "/jobs", "/graph", "/conflicts", "/renames", "/system":
		return true
	default:
		return strings.HasPrefix(path, "/app/")
	}
}
