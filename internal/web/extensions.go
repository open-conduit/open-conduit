package web

import (
	"context"

	"github.com/open-conduit/open-conduit/client"
)

// panelView is a panel with what its buttons need: the page and the target.
type panelView struct {
	client.Panel
	Page            *page
	ScopeID, WorkID string
}

// panels fetches a target's panels for p. A daemon too old to serve them
// shows none: its actions say it needs upgrading.
func (s *Server) panels(ctx context.Context, p *page, t client.ActionTarget) ([]panelView, error) {
	ps, err := s.src.Panels(ctx, t)
	if tooOld(err) {
		return nil, nil
	}
	out := make([]panelView, len(ps))
	for i, x := range ps {
		out[i] = panelView{Panel: x, Page: p, ScopeID: t.ScopeID, WorkID: t.WorkID}
	}
	return out, err
}

// unshown are the actions no panel offers.
func unshown(actions []client.Action, panels []panelView) []client.Action {
	in := map[[2]string]bool{}
	for _, p := range panels {
		for _, sec := range p.Sections {
			for _, a := range sec.Actions {
				in[[2]string{a.Provider, a.ID}] = true
			}
		}
	}
	var out []client.Action
	for _, a := range actions {
		if !in[[2]string{a.Provider, a.ID}] {
			out = append(out, a)
		}
	}
	return out
}
