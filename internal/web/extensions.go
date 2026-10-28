package web

import (
	"context"
	"strings"

	"github.com/open-conduit/open-conduit/client"
)

// What extensions contribute, rendered as data: panels and actions. Nothing
// here or in panels.html names an extension, so a new one shows up without
// changes. Panels follow extension/panel.go: each section's title, text,
// fields, table, links and action controls, in that order.

// panelView is a panel with what its controls need: the page and the
// target. Prominent panels (those offering an action, above a Work's score
// and on Needs you) are drawn heavier.
type panelView struct {
	client.Panel
	Page            *page
	ScopeID, WorkID string
	Prominent       bool
}

// actionView is one action control with its target and page. Back, when
// set, is where the change returns to instead of the page it is on (a
// confirmation page returns to where it was opened from).
type actionView struct {
	client.Action
	Page            *page
	ScopeID, WorkID string
	Back            string
}

// Return is where the action's change returns to.
func (a actionView) Return() string {
	if a.Back != "" {
		return a.Back
	}
	return a.Page.Back
}

// Hint is the control's one-line instruction: "label — prompt".
func (a actionView) Hint() string {
	if a.Prompt == "" || a.Prompt == a.Label {
		return a.Label
	}
	return a.Label + " — " + a.Prompt
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

// workPanels are w's panels for p: which is "acting" for those offering an
// action (drawn prominent), "rest" for the others, or "all".
func workPanels(w workView, p *page, which string) []panelView {
	var out []panelView
	for _, x := range w.Panels {
		acts := panelActs(x)
		if which == "acting" && !acts || which == "rest" && acts {
			continue
		}
		out = append(out, panelView{Panel: x, Page: p, WorkID: w.ID, Prominent: acts})
	}
	return out
}

// actionIn is a panel section's action with the panel's target.
func actionIn(a client.Action, v panelView) actionView {
	return actionView{Action: a, Page: v.Page, ScopeID: v.ScopeID, WorkID: v.WorkID}
}

// unshownOn are the actions offered on w that none of its panels shows,
// as controls on page p.
func unshownOn(w workView, p *page) []actionView {
	var ps []panelView
	for _, x := range w.Panels {
		ps = append(ps, panelView{Panel: x})
	}
	var out []actionView
	for _, a := range unshown(w.Actions, ps) {
		out = append(out, actionView{Action: a, Page: p, WorkID: w.ID})
	}
	return out
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

// paras splits a panel's text into paragraphs at blank lines.
func paras(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
