package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/alpkeskin/rota/core/internal/models"
)

// writeJSON writes v as a JSON response with the given status code. It is the
// single response helper for the handlers package — previously every handler
// carried its own identical jsonResponse/errorResponse pair.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// writeError writes a JSON error response ({"error": message}). The message is
// JSON-encoded (not string-interpolated), so text containing quotes or newlines
// can't produce a malformed body, and the Content-Type is correctly JSON —
// unlike http.Error, which mislabels the body as text/plain.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, models.ErrorResponse{Error: message})
}
