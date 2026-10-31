package setup

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Outline is the wiring of a configuration with no opaque config and no
// references: what GET /config shows of it.
type Outline struct {
	Connections []OutlineConnection `json:"connections"`
	Scopes      []OutlineScope      `json:"scopes"`
}

type OutlineConnection struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type OutlineScope struct {
	ID         string   `json:"id"`
	Name       string   `json:"name,omitempty"`
	Parent     string   `json:"parent_id,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
	Bindings   []string `json:"bindings,omitempty"`
	Routes     []string `json:"routes,omitempty"`
}

// outline reads a config document's wiring: connection IDs and types, scope
// IDs and names, entry types, binding and route names.
func outline(doc map[string]any) Outline {
	out := Outline{Connections: []OutlineConnection{}, Scopes: []OutlineScope{}}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	list := func(v any) []map[string]any {
		var out []map[string]any
		items, _ := v.([]any)
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	for _, c := range list(doc["connections"]) {
		out.Connections = append(out.Connections, OutlineConnection{ID: str(c, "id"), Type: str(c, "type")})
	}
	for _, s := range list(doc["scopes"]) {
		o := OutlineScope{ID: str(s, "id"), Name: str(s, "name"), Parent: str(s, "parent_id")}
		for _, e := range list(s["extensions"]) {
			o.Extensions = append(o.Extensions, str(e, "name")+" ("+str(e, "type")+")")
		}
		for _, b := range list(s["bindings"]) {
			o.Bindings = append(o.Bindings, str(b, "name")+" ("+str(b, "executor")+")")
		}
		for _, r := range list(s["routes"]) {
			o.Routes = append(o.Routes, str(r, "name")+" ("+str(r, "handler")+")")
		}
		out.Scopes = append(out.Scopes, o)
	}
	return out
}

// touched are the scopes and connections a change set's changes reach.
func touched(cs changeSet) (scopes, connections map[string]bool) {
	scopes, connections = map[string]bool{}, map[string]bool{}
	for _, c := range cs.Changes {
		parts := strings.Split(c.Pointer, "/")
		if len(parts) < 2 {
			continue
		}
		id := ""
		if len(parts) >= 3 {
			id = strings.ReplaceAll(strings.ReplaceAll(parts[2], "~1", "/"), "~0", "~")
		}
		if id == "-" || id == "" {
			// An appended scope or connection: its own id says which.
			var v struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(c.Value, &v)
			id = v.ID
		}
		switch parts[1] {
		case "scopes":
			scopes[id] = true
		case "connections":
			connections[id] = true
		}
	}
	return scopes, connections
}

// read returns what a session may read of the configuration: the outline of
// the candidate (the base plus the approved changes), its settings, and in
// full the scopes and connections the change set touches, each redacted
// (secret_refs values become [redacted]). Nothing else of the config.
func (t *Tools) read(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
	var input struct{}
	if len(in.Data) > 0 {
		if err := decode(in.Data, &input, "{}"); err != nil {
			return domain.Payload{}, err
		}
	}
	base, err := t.o.Config.Base(ctx)
	if err != nil {
		return domain.Payload{}, refuse("unavailable", "the configuration can't be read now")
	}
	var text []byte
	var cs changeSet
	var st map[string]string
	if err = t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		w, err := t.session(tx, a)
		if err != nil {
			return err
		}
		if cs, err = t.changeSetFor(ctx, tx, w, base); err != nil {
			return err
		}
		if st, err = states(tx, w, cs); err != nil {
			return err
		}
		cand, _, _ := candidate(ctx, t.o.Config, cs, st)
		text = cand.Text
		return nil
	}); err != nil {
		return domain.Payload{}, err
	}
	var doc map[string]any
	if json.Unmarshal(text, &doc) != nil {
		return domain.Payload{}, refuse("unavailable", "the configuration can't be read now")
	}
	scopes, connections := touched(cs)
	full := map[string]json.RawMessage{}
	for _, kind := range []string{"scopes", "connections"} {
		items, _ := doc[kind].([]any)
		for _, it := range items {
			m, _ := it.(map[string]any)
			id, _ := m["id"].(string)
			if (kind == "scopes" && scopes[id]) || (kind == "connections" && connections[id]) {
				b, _ := json.Marshal(m)
				full[kind+"/"+id] = redact("/"+kind+"/-", b)
			}
		}
	}
	changes := []map[string]any{}
	for _, c := range cs.Changes {
		changes = append(changes, map[string]any{"change": c.ID, "op": c.Op, "pointer": c.Pointer, "state": st[c.ID],
			"before": redact(c.Pointer, c.Before), "after": redact(c.Pointer, c.Value)})
	}
	settings, _ := json.Marshal(doc["settings"])
	out := map[string]any{"generation": cs.BaseGeneration, "outline": outline(doc), "settings": json.RawMessage(settings), "changes": changes,
		"note": "this is the configuration with the approved changes; scopes and connections your changes touch are shown in full, reference text redacted"}
	if len(full) > 0 {
		keys := make([]string, 0, len(full))
		for k := range full {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ordered := map[string]json.RawMessage{}
		for _, k := range keys {
			ordered[k] = full[k]
		}
		out["full"] = ordered
	}
	if looksLikeSecret(out) {
		return domain.Payload{}, refuse("looks_like_secret", "the configuration holds text that looks like a secret; ask the operator to replace it with a reference")
	}
	return payload(SchemaTool, out), nil
}
