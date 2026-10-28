package web

import "net/http"

// attentionPage is Needs you (10-08). Placeholder until the port lands.
func (s *Server) attentionPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	s.render(w, r, "attention", &page{Title: "Needs you", Nav: "attention"}, s.cachedWorld(ctx, true))
}
