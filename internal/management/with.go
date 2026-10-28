package management

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
)

// List forms (issue #124): GET /work?with=actions,panels and
// GET /events?with=route add to each listed item what its single-target
// endpoint returns, so a view needs one request per page rather than one
// per item. Actions and panels call every provider for every Work on the
// page, so a page that asks for them holds at most maxWithLimit items.

const maxWithLimit = 50

// with parses the with= query parameter: a comma-separated set drawn from
// allowed.
func with(q listQuery, allowed ...string) (map[string]bool, error) {
	v, ok := q.filters["with"]
	if !ok {
		return nil, nil
	}
	out := map[string]bool{}
	for _, part := range strings.Split(v, ",") {
		known := false
		for _, a := range allowed {
			known = known || part == a
		}
		if !known || out[part] {
			return nil, fail(400, "invalid_filter", "with takes "+strings.Join(allowed, ", "))
		}
		out[part] = true
	}
	return out, nil
}

// wireWorkWith is a listed Work with what with= asked for. A nil field was
// not asked for; an asked-for field is a list, empty when there is nothing.
type wireWorkWith struct {
	wireWork
	Actions *[]wireAction `json:"actions,omitempty"`
	Panels  *[]wirePanel  `json:"panels,omitempty"`
}

// decorate adds each listed Work's actions and panels, computed as
// GET /work/{id}/actions and /panels compute them.
func (s *Server) decorate(ctx context.Context, items []any, ask map[string]bool, works map[string]domain.Work, scopes map[domain.ID]domain.Scope) {
	for i, it := range items {
		ww, ok := it.(wireWork)
		if !ok {
			continue
		}
		out := wireWorkWith{wireWork: ww}
		w, okw := works[ww.ID]
		sc, oks := scopes[w.ScopeID]
		t := extension.ActionTarget{Scope: sc, Work: &w}
		if ask["actions"] {
			a := []wireAction{}
			if okw && oks {
				a = s.available(ctx, t)
			}
			out.Actions = &a
		}
		if ask["panels"] {
			p := []wirePanel{}
			if okw && oks {
				p = s.panels(ctx, t)
			}
			out.Panels = &p
		}
		items[i] = out
	}
}

// wireEventWith is a listed Event with its routing explanation.
type wireEventWith struct {
	wireEvent
	Route *wireEventRoute `json:"route,omitempty"`
}

// ---- configuration summaries ----

// maxSummary bounds a config summary's entries and each value's length.
const maxSummary = 50

// sensitiveKey names a config field whose value is never shown, whatever
// it holds.
func sensitiveKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range []string{"secret", "token", "password", "passwd", "credential", "private", "api_key", "apikey"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// summarize lists an opaque config object's top-level fields for the
// operator's view of the config: scalars and lists of scalars as written,
// nested objects and lists by size. A field whose name looks sensitive, or
// whose value is or contains secret reference text, is "[redacted]".
func summarize(raw json.RawMessage, refs []string) []wireEntry {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var m map[string]any
	if d.Decode(&m) != nil || len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxSummary {
		keys = keys[:maxSummary]
	}
	out := make([]wireEntry, 0, len(keys))
	for _, k := range keys {
		v := describeValue(m[k])
		if sensitiveKey(k) || secretText(v, refs) {
			v = "[redacted]"
		}
		out = append(out, wireEntry{Name: k, Detail: clip(v)})
	}
	return out
}

func describeValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "true"
		}
		return "false"
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			switch e.(type) {
			case map[string]any, []any:
				return plural(len(x), "item")
			}
			parts = append(parts, describeValue(e))
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		return plural(len(x), "field")
	}
	return ""
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

func clip(s string) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) > 200 {
		s = strings.ToValidUTF8(s[:200], "") + "…"
	}
	return s
}

// secretText reports whether v is, or contains, secret reference text: a
// reference the config names, or any string shaped like one.
func secretText(v string, refs []string) bool {
	for _, r := range refs {
		if r != "" && strings.Contains(v, r) {
			return true
		}
	}
	for _, part := range strings.Split(v, ", ") {
		if config.ValidSecretRef(strings.TrimSpace(part)) {
			return true
		}
	}
	return false
}

// secretRefs is every reference text in the snapshot, to keep out of
// summaries.
func secretRefs(snap *config.Snapshot) []string {
	var out []string
	for _, c := range snap.Connections {
		for _, r := range c.SecretRefs {
			out = append(out, r)
		}
	}
	for _, sc := range snap.Scopes {
		for _, r := range sc.SecretRefs {
			out = append(out, r)
		}
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
