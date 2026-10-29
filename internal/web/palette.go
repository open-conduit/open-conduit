package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// paletteItem is one thing the jump palette offers: labels and a URL only.
type paletteItem struct {
	Label string `json:"label"`
	Hint  string `json:"hint"`
	Href  string `json:"href"`
	// Mark is the item's state mark, drawn by the server.
	Mark string `json:"mark,omitempty"`
	// Find is more text the item matches, unshown: a Work's summary.
	Find string `json:"find,omitempty"`
}

// palette feeds the jump palette from the cached read of every Work: each
// Work, then each scope, then the pages.
func (s *Server) palette(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	wd := s.cachedWorld(ctx, false)
	var items []paletteItem
	for _, v := range wd.Works {
		items = append(items, paletteItem{Label: v.Label(), Hint: v.ScopeID + " · " + strings.ToLower(v.Disp.Label), Href: "/work?" + url.Values{"id": {v.ID}}.Encode(), Mark: string(markSVG(v.Disp.Kind, 12, "")), Find: v.Summary})
	}
	for _, sc := range wd.Scopes {
		items = append(items, paletteItem{Label: sc.Name, Hint: "scope", Href: "/config?" + url.Values{"scope": {sc.ID}}.Encode()})
	}
	items = append(items,
		paletteItem{Label: "Runs", Hint: "page", Href: "/"},
		paletteItem{Label: "Needs you", Hint: "page", Href: "/attention"},
		paletteItem{Label: "Configuration", Hint: "page", Href: "/config"},
		paletteItem{Label: "Setup", Hint: "page", Href: "/setup"},
	)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}
