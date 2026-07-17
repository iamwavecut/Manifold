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
	assets, _ := fs.Sub(files, ".")
	fileServer := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.Redirect(w, r, "/app/", http.StatusTemporaryRedirect)
		case "/assets/styles.css":
			r.URL.Path = "/styles.css"
			fileServer.ServeHTTP(w, r)
		case "/assets/app.js":
			r.URL.Path = "/dist/app.js"
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			fileServer.ServeHTTP(w, r)
		default:
			if strings.HasPrefix(r.URL.Path, "/app") {
				data, _ := files.ReadFile("index.html")
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(data)
				return
			}
			http.NotFound(w, r)
		}
	})
}
