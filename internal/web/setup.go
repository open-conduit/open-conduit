package web

import (
	"net/http"

	"github.com/open-conduit/open-conduit/client"
)

// quickStart is the docs page that sets Open Conduit up from a terminal.
const quickStart = "https://open-conduit.github.io/open-conduit/start/quick-start/"

type setupData struct {
	Config     client.Config
	Generation string
	QuickStart string
}

// setupPage is Setup. The prototype's assistant runs a setup session in the
// browser; the daemon does not offer setup sessions yet (issue #127), so
// the page says what is set up and how to add to it from a terminal with
// the same playbook, rather than imitating the session.
func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	p := &page{Title: "Setup", Nav: "setup"}
	d := setupData{QuickStart: quickStart}
	cfg, err := s.src.Config(ctx)
	if err != nil && !tooOld(err) {
		p.Err = errText(err)
	}
	d.Config, d.Generation = cfg, cfg.Generation
	s.render(w, r, "setup", p, d)
}
