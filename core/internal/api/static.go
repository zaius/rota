package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// spaHandler serves the built dashboard from webDir. Requests that map to a real
// file are served directly (with far-future caching for fingerprinted assets);
// everything else falls back to index.html so client-side routes resolve. API
// and WebSocket paths are never served here — they are registered routes, and
// an unmatched one under those prefixes returns 404 rather than the SPA shell.
func spaHandler(webDir string) http.HandlerFunc {
	fileServer := http.FileServer(http.Dir(webDir))
	indexPath := filepath.Join(webDir, "index.html")

	return func(w http.ResponseWriter, r *http.Request) {
		reqPath := r.URL.Path
		if strings.HasPrefix(reqPath, "/api/") || strings.HasPrefix(reqPath, "/ws/") {
			http.NotFound(w, r)
			return
		}

		clean := filepath.Clean(reqPath)
		full := filepath.Join(webDir, clean)

		// Guard against path traversal escaping webDir.
		if !strings.HasPrefix(full, filepath.Clean(webDir)) {
			http.NotFound(w, r)
			return
		}

		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			if strings.HasPrefix(clean, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		// Unknown path → SPA shell for client-side routing.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, indexPath)
	}
}
