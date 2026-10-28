package management

import (
	"context"
	"sort"

	"github.com/open-conduit/open-conduit/extension"
)

// Extension panels (issue #90): GET /scopes/{id}/panels and
// GET /work/{id}/panels return what each action provider that is also an
// extension.PanelProvider shows on the target now, as a constrained
// document every operator view renders the same way. A panel that fails
// extension.Panel.Validate is left out whole, and so is everything from a
// provider that fails. A button is resolved to the provider's action as
// listed on the target now, and left out when it is not listed, so a panel
// can offer only what the action path would accept.

type wirePanel struct {
	Provider string        `json:"provider"`
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Sections []wireSection `json:"sections"`
}

type wireSection struct {
	Title   string       `json:"title,omitempty"`
	Text    string       `json:"text,omitempty"`
	Fields  []wireField  `json:"fields,omitempty"`
	Table   *wireTable   `json:"table,omitempty"`
	Links   []wireLink   `json:"links,omitempty"`
	Actions []wireAction `json:"actions,omitempty"`
}

type wireField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type wireTable struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

type wireLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

func (s *Server) listScopePanels(c *call) (int, any, error) { return s.listPanels(c, false) }
func (s *Server) listWorkPanels(c *call) (int, any, error)  { return s.listPanels(c, true) }

func (s *Server) listPanels(c *call, work bool) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	t, err := s.actionTarget(c, work)
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"panels": s.panels(c.ctx, t)}, nil
}

// panels gathers every provider's panels on t, sorted by provider.
func (s *Server) panels(ctx context.Context, t extension.ActionTarget) []wirePanel {
	ps := s.providers(ctx, t.Scope.ID)
	names := make([]string, 0, len(ps))
	for name := range ps {
		names = append(names, name)
	}
	sort.Strings(names)
	out := []wirePanel{}
	for _, name := range names {
		pp, ok := ps[name].(extension.PanelProvider)
		if !ok {
			continue
		}
		panels, err := pp.Panels(ctx, t)
		if err != nil || len(panels) == 0 || len(panels) > extension.MaxPanels {
			continue
		}
		var actions map[string]wireAction
		seen := map[string]bool{}
		for _, p := range panels {
			if p.Validate() != nil || seen[p.ID] {
				continue
			}
			seen[p.ID] = true
			if actions == nil && refersToActions(p) {
				actions = map[string]wireAction{}
				for _, a := range offered(ctx, name, ps[name], t) {
					actions[a.ID] = a
				}
			}
			out = append(out, toPanel(name, p, actions))
		}
	}
	return out
}

func refersToActions(p extension.Panel) bool {
	for _, s := range p.Sections {
		if len(s.Actions) > 0 {
			return true
		}
	}
	return false
}

func toPanel(provider string, p extension.Panel, actions map[string]wireAction) wirePanel {
	out := wirePanel{Provider: provider, ID: p.ID, Title: p.Title, Sections: make([]wireSection, 0, len(p.Sections))}
	for _, sec := range p.Sections {
		w := wireSection{Title: sec.Title, Text: sec.Text}
		for _, f := range sec.Fields {
			w.Fields = append(w.Fields, wireField{Name: f.Name, Value: f.Value})
		}
		if t := sec.Table; t != nil {
			w.Table = &wireTable{Columns: t.Columns, Rows: t.Rows}
			if w.Table.Rows == nil {
				w.Table.Rows = [][]string{}
			}
		}
		for _, l := range sec.Links {
			w.Links = append(w.Links, wireLink{Label: l.Label, URL: l.URL})
		}
		for _, id := range sec.Actions {
			if a, ok := actions[id]; ok {
				w.Actions = append(w.Actions, a)
			}
		}
		out.Sections = append(out.Sections, w)
	}
	return out
}
