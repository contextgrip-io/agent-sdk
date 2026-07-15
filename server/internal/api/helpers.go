package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// errorBody matches the Error schema in openapi.yaml.
type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: message, Code: code})
}

// decodeJSON reads a bounded JSON request body.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}
