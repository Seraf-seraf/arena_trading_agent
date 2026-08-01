// Package dashboard serves the local controller UI.
package dashboard

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var index []byte

// Handler returns the self-contained dashboard.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const methodCtx = "dashboard.Handler.ServeHTTP"

		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, methodCtx+": метод HTTP не поддерживается", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/dashboard" {
			http.Error(w, methodCtx+": запрошенная страница не найдена", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(index)
	})
}
