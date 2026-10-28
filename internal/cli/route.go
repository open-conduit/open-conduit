package cli

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/client"
)

// routeExplanation is `route KEY`: the Work owning the key and, for each
// Event correlated with it, how the router decided (the durable route
// decision, so the explanation is what execution actually used).
type routeExplanation struct {
	Key    string        `json:"key"`
	Works  []routeWork   `json:"works"`
	Events []routedEvent `json:"events"`
	Note   string        `json:"note,omitempty"`
}

type routeWork struct {
	ID    string       `json:"id"`
	Kind  string       `json:"kind"`
	Scope string       `json:"scope_id"`
	State string       `json:"state"`
	Keys  []client.Key `json:"correlation_keys"`
}

type routedEvent struct {
	Sequence string      `json:"sequence"`
	Key      *client.Key `json:"correlation_key,omitempty"`
	client.EventRoute
}

// keyMatches reports whether an external ID names the key: the ID itself, an
// issue key (any generation), or a number (#12 or 12).
func keyMatches(external, key string) bool {
	k := strings.TrimPrefix(key, "#")
	if external == key || external == k {
		return true
	}
	for _, prefix := range []string{"issue/", "pr/"} {
		if external == prefix+k || strings.HasPrefix(external, prefix+k+"/generation/") || strings.HasPrefix(external, prefix+strings.ToUpper(k)+"/generation/") || external == prefix+strings.ToUpper(k) {
			return true
		}
	}
	return false
}

func (o *operator) route(args []string) int {
	for i, a := range args {
		if a == "--samples" || a == "-samples" {
			return o.routeSamples(append(append([]string(nil), args[:i]...), args[i+1:]...))
		}
		if a == "--trace" || a == "-trace" || strings.HasPrefix(a, "--trace=") || strings.HasPrefix(a, "-trace=") {
			return o.routeTrace(args)
		}
	}
	f := newFlags("route", "scope", "limit")
	pos, err := f.parse(args)
	if err != nil || len(pos) != 1 || pos[0] == "" {
		return usageError(o.errOut, "expected route KEY [--scope S] [--limit N]")
	}
	limit := 2000
	if v := f.get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 100000 {
			return usageError(o.errOut, "--limit must be 1-100000")
		}
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	cx, cancel := ctx()
	defer cancel()
	key := pos[0]
	out := routeExplanation{Key: key, Works: []routeWork{}, Events: []routedEvent{}}
	owned := map[string]bool{}
	q := url.Values{"limit": {"200"}}
	if s := f.get("scope"); s != "" {
		q.Set("scope_id", s)
	}
	for seen := 0; seen < limit; {
		page, err := c.Works(cx, q)
		if err != nil {
			return o.emit(nil, err)
		}
		for _, w := range page.Items {
			seen++
			for _, k := range w.CorrelationKeys {
				if keyMatches(k.ExternalID, key) {
					out.Works = append(out.Works, routeWork{ID: w.ID, Kind: w.Kind, Scope: w.ScopeID, State: w.State, Keys: w.CorrelationKeys})
					owned[w.ID] = true
					break
				}
			}
		}
		if page.NextCursor == "" {
			break
		}
		q.Set("cursor", page.NextCursor)
	}
	eq := url.Values{"limit": {"200"}}
	if s := f.get("scope"); s != "" {
		eq.Set("scope_id", s)
	}
	scanned := 0
	for scanned < limit {
		page, err := c.Events(cx, eq)
		if err != nil {
			return o.emit(nil, err)
		}
		for _, e := range page.Items {
			scanned++
			if !(e.CorrelationKey != nil && keyMatches(e.CorrelationKey.ExternalID, key)) && !owned[e.RoutedWorkID] && !owned[e.WorkID] {
				continue
			}
			r, err := c.EventRoute(cx, e.ID)
			if err != nil {
				return o.emit(nil, err)
			}
			out.Events = append(out.Events, routedEvent{Sequence: e.Sequence, Key: e.CorrelationKey, EventRoute: r})
		}
		if page.NextCursor == "" {
			break
		}
		eq.Set("cursor", page.NextCursor)
	}
	if scanned >= limit {
		out.Note = "stopped after --limit events; raise it to see older history"
	}
	if len(out.Works) == 0 && len(out.Events) == 0 {
		out.Note = "no Work or Event carries that key"
	}
	return o.emit(out, nil)
}

// routeSamples is `route --samples --scope S`: the sample Events the scope's
// route handlers offer for --trace.
func (o *operator) routeSamples(args []string) int {
	f := newFlags("route", "scope")
	pos, err := f.parse(args)
	if err != nil || len(pos) != 0 || f.get("scope") == "" {
		return usageError(o.errOut, "expected route --samples --scope S")
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	cx, cancel := ctx()
	defer cancel()
	return o.emit(c.RouteSamples(cx, f.get("scope")))
}

// routeTrace is `route --trace TYPE|SAMPLE --scope S`: a dry run of how a
// hypothetical Event would route now. A sample (by its ID, or the part after
// the handler) supplies the Event; the other flags override its fields.
func (o *operator) routeTrace(args []string) int {
	f := newFlags("route", "trace", "scope", "source", "key-source", "key-external-id", "work-id", "schema", "data")
	pos, err := f.parse(args)
	scope, name := f.get("scope"), f.get("trace")
	if err != nil || len(pos) != 0 || scope == "" || name == "" {
		return usageError(o.errOut, "expected route --trace TYPE|SAMPLE --scope S [--source SRC] [--key-source S --key-external-id X] [--work-id W] [--schema S --data JSON]")
	}
	if (f.get("key-source") == "") != (f.get("key-external-id") == "") {
		return usageError(o.errOut, "--key-source and --key-external-id go together")
	}
	payload, err := f.payload()
	if err != nil {
		return usageError(o.errOut, err.Error())
	}
	c, _, err := o.client()
	if err != nil {
		return o.emit(nil, err)
	}
	cx, cancel := ctx()
	defer cancel()
	e := client.TraceEvent{Type: name}
	if !strings.Contains(name, ".") || strings.Contains(name, "/") {
		samples, err := c.RouteSamples(cx, scope)
		if err != nil {
			return o.emit(nil, err)
		}
		found := false
		for _, s := range samples {
			if s.ID == name || strings.TrimPrefix(s.ID, s.Handler+"/") == name {
				e, found = s.TraceEvent, true
				break
			}
		}
		if !found {
			return usageError(o.errOut, "no sample "+name+" in scope "+scope+"; see route --samples --scope "+scope)
		}
	}
	if v := f.get("source"); v != "" {
		e.Source = v
	}
	if v := f.get("key-source"); v != "" {
		e.CorrelationKey = &client.Key{Source: v, ExternalID: f.get("key-external-id")}
	}
	if v := f.get("work-id"); v != "" {
		e.WorkID = v
	}
	if payload != nil {
		e.Data = payload
	}
	return o.emit(c.TraceRoute(cx, scope, e))
}
