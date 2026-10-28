package web

import "net/http"

// palette feeds the jump palette (10-10). Placeholder.
func (s *Server) palette(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("[]"))
}
