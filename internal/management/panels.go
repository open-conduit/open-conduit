package management

import (
	"time"

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
	Title    string        `json:"title,omitempty"`
	Text     string        `json:"text,omitempty"`
	Fields   []wireField   `json:"fields,omitempty"`
	Table    *wireTable    `json:"table,omitempty"`
	Messages []wireMessage `json:"messages,omitempty"`
	Links    []wireLink    `json:"links,omitempty"`
	Actions  []wireAction  `json:"actions,omitempty"`
}

type wireMessage struct {
	From    string    `json:"from"`
	Label   string    `json:"label,omitempty"`
	Text    string    `json:"text"`
	At      time.Time `json:"at,omitzero"`
	Choices []string  `json:"choices,omitempty"`
	Chosen  string    `json:"chosen,omitempty"`
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
	v, cancel := s.viewer(c.ctx)
	defer cancel()
	return 200, map[string]any{"panels": v.view(t, false, true).panels}, nil
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
		for _, m := range sec.Messages {
			w.Messages = append(w.Messages, wireMessage(m))
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
