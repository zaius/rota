package handlers

import (
	"net/http"
	"time"

	"github.com/alpkeskin/rota/core/internal/version"
)

var startTime = time.Now()

func Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "healthy",
		"version": version.Version,
		"uptime":  int(time.Since(startTime).Seconds()),
	})
}
