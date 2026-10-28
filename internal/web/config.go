package web

import (
	"net/http"

	"github.com/open-conduit/open-conduit/client"
)

// flowScope is one scope's wiring as the config page draws it, in rows
// so a route and its binding sit level. Sources are its ingress extensions
// and "by hand"; each feeds every route, tried in order. Other Work names
// its binding itself, so it gets its own row, beside the bindings no
// route uses.
type flowScope struct {
	Scope    client.ConfigScope
	Rows     []flowRow
	Others   []client.ConfigExtension
	Unrouted []client.ConfigBinding
}

// flowRow is one row of the diagram: a source, a route, or both. First
// and Last mark where the line joining the sources to the routes ends.
type flowRow struct {
	Source      *flowSource
	Route       *client.ConfigRoute
	Binding     *client.ConfigBinding
	First, Last bool
}

type flowSource struct {
	Name, Kind, Via string
	Entries         []client.ConfigEntry
}

type configData struct {
	Config client.Config
	Scopes []flowScope
}

func flow(sc client.ConfigScope) flowScope {
	f := flowScope{Scope: sc}
	var sources []flowSource
	for _, x := range sc.Extensions {
		if x.Ingress {
			sources = append(sources, flowSource{Name: x.Name, Kind: x.Type, Via: x.Connection, Entries: x.Entries})
		} else {
			f.Others = append(f.Others, x)
		}
	}
	sources = append(sources, flowSource{Name: "By hand", Kind: "actions and events from the web view, TUI or CLI"})
	used := map[string]bool{}
	for _, r := range sc.Routes {
		used[r.Binding] = true
	}
	for _, b := range sc.Bindings {
		if !used[b.Name] {
			f.Unrouted = append(f.Unrouted, b)
		}
	}
	n := max(len(sources), len(sc.Routes))
	for i := range n {
		row := flowRow{First: i == 0, Last: i == n-1}
		if i < len(sources) {
			row.Source = &sources[i]
		}
		if i < len(sc.Routes) {
			r := &sc.Routes[i]
			row.Route = r
			for j := range sc.Bindings {
				if sc.Bindings[j].Name == r.Binding {
					row.Binding = &sc.Bindings[j]
				}
			}
		}
		f.Rows = append(f.Rows, row)
	}
	return f
}

func (s *Server) configPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	p := &page{Title: "Config", Nav: "config"}
	cfg, err := s.src.Config(ctx)
	switch {
	case tooOld(err):
		p.Err = "this daemon does not show its config; upgrade it to see it here"
	case err != nil:
		p.Err = errText(err)
	}
	d := configData{Config: cfg}
	for _, sc := range cfg.Scopes {
		d.Scopes = append(d.Scopes, flow(sc))
	}
	s.render(w, r, "config", p, d)
}

// back is where a change returns to: the page it was made on.

// reconcileScope asks for an immediate reconcile of a scope's sources
// (10-09). Placeholder.
func (s *Server) reconcileScope(w http.ResponseWriter, r *http.Request) {
	http.NotFound(w, r)
}
