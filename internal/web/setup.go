package web

import "net/http"

// setupPage is Setup (10-11).
func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "setup", &page{Title: "Setup", Nav: "setup"}, nil)
}
