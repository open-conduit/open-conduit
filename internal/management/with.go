package management

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// List forms (issue #124): GET /work?with=actions,panels and
// GET /events?with=route add to each listed item what its single-target
// endpoint returns, so a view needs one request per page rather than one
// per item. Actions and panels come from extension providers, so a page
// that asks for them holds at most maxWithLimit Work, decorated
// viewWorkers at a time within one ViewDeadline.

const (
	maxWithLimit = 50
	viewWorkers  = 6
	// DefaultProviderTimeout bounds one provider call for actions or panels.
	DefaultProviderTimeout = 2 * time.Second
	// DefaultViewDeadline bounds every provider call one request makes.
	DefaultViewDeadline = 4 * time.Second
)

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

// viewer computes the actions and panels providers offer on targets, for
// the single-target endpoints and the list forms alike. Every provider
// call is bounded by the provider timeout and the request's view
// deadline. A provider whose Actions or Panels call fails or times out is
// not asked for that again during the request, and contributes none of it
// to any target of the request.
type viewer struct {
	s      *Server
	ctx    context.Context // done at the view deadline
	mu     sync.Mutex
	failed map[string]bool // provider + "\x00" + call
	// gates hold a provider call's first use on a page, so concurrent
	// targets ask a failing or hung provider once, not once each; proven
	// calls run concurrently.
	gates  map[string]chan struct{}
	proven map[string]bool
}

func (s *Server) viewer(ctx context.Context) (*viewer, func()) {
	ctx, cancel := context.WithCancel(ctx)
	deadline := s.o.ViewDeadline
	if deadline == nil {
		deadline = func() <-chan time.Time { return time.After(DefaultViewDeadline) }
	}
	timer := deadline()
	go func() {
		select {
		case <-timer:
			cancel()
		case <-ctx.Done():
		}
	}()
	return &viewer{s: s, ctx: ctx, failed: map[string]bool{}, gates: map[string]chan struct{}{}, proven: map[string]bool{}}, cancel
}

func (v *viewer) hasFailed(provider, call string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.failed[provider+"\x00"+call]
}

// providerCall runs one provider call within the timeouts, reporting whether it
// succeeded.
func providerCall[T any](v *viewer, provider, name string, fn func(context.Context) (T, error)) (T, bool) {
	var zero T
	key := provider + "\x00" + name
	v.mu.Lock()
	gate, ok := v.gates[key]
	if !ok {
		gate = make(chan struct{}, 1)
		v.gates[key] = gate
	}
	proven := v.proven[key]
	v.mu.Unlock()
	if !proven {
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		case <-v.ctx.Done():
			return zero, false
		}
	}
	if v.hasFailed(provider, name) || v.ctx.Err() != nil {
		return zero, false
	}
	ctx, cancel := context.WithCancel(v.ctx)
	defer cancel()
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := fn(ctx)
		done <- result{r, err}
	}()
	timeout := v.s.o.ProviderTimeout
	if timeout == nil {
		timeout = func(string) <-chan time.Time { return time.After(DefaultProviderTimeout) }
	}
	select {
	case r := <-done:
		if r.err == nil {
			v.mu.Lock()
			v.proven[key] = true
			v.mu.Unlock()
			return r.v, true
		}
	case <-timeout(provider):
	case <-v.ctx.Done():
		return zero, false
	}
	v.mu.Lock()
	v.failed[key] = true
	v.mu.Unlock()
	return zero, false
}

// targetView is what providers offer on one target.
type targetView struct {
	actions []wireAction
	panels  []wirePanel
}

// view asks t's providers for their actions and panels, as asked. A panel
// button resolves to its provider's action as listed on t now. A target
// the view deadline overtakes gets nothing.
func (v *viewer) view(t extension.ActionTarget, actions, panels bool) targetView {
	out := targetView{actions: []wireAction{}, panels: []wirePanel{}}
	ps := v.s.providers(v.ctx, t.Scope.ID)
	names := make([]string, 0, len(ps))
	for name := range ps {
		names = append(names, name)
	}
	sort.Strings(names)
	listed := map[string][]wireAction{}
	asked := map[string]bool{}
	actionsOf := func(name string) []wireAction {
		if !asked[name] {
			asked[name] = true
			raw, ok := providerCall(v, name, "actions", func(ctx context.Context) ([]extension.Action, error) { return ps[name].Actions(ctx, t) })
			if ok {
				listed[name] = wellFormed(name, raw)
			}
		}
		return listed[name]
	}
	for _, name := range names {
		if actions {
			out.actions = append(out.actions, actionsOf(name)...)
		}
		pp, ok := ps[name].(extension.PanelProvider)
		if !panels || !ok {
			continue
		}
		got, ok := providerCall(v, name, "panels", func(ctx context.Context) ([]extension.Panel, error) { return pp.Panels(ctx, t) })
		if !ok || len(got) == 0 || len(got) > extension.MaxPanels {
			continue
		}
		var buttons map[string]wireAction
		seen := map[string]bool{}
		for _, p := range got {
			if p.Validate() != nil || seen[p.ID] {
				continue
			}
			seen[p.ID] = true
			if buttons == nil && refersToActions(p) {
				buttons = map[string]wireAction{}
				for _, a := range actionsOf(name) {
					buttons[a.ID] = a
				}
			}
			out.panels = append(out.panels, toPanel(name, p, buttons))
		}
	}
	if v.ctx.Err() != nil {
		return targetView{actions: []wireAction{}, panels: []wirePanel{}}
	}
	sort.SliceStable(out.actions, func(i, j int) bool {
		if out.actions[i].Provider != out.actions[j].Provider {
			return out.actions[i].Provider < out.actions[j].Provider
		}
		return out.actions[i].ID < out.actions[j].ID
	})
	return out
}

// prune drops from tv what came from a provider that failed later in the
// request, so a failing provider contributes nothing to any target.
func (v *viewer) prune(tv targetView) targetView {
	out := targetView{actions: []wireAction{}, panels: []wirePanel{}}
	for _, a := range tv.actions {
		if !v.hasFailed(a.Provider, "actions") {
			out.actions = append(out.actions, a)
		}
	}
	for _, p := range tv.panels {
		if v.hasFailed(p.Provider, "panels") {
			continue
		}
		if v.hasFailed(p.Provider, "actions") {
			for i := range p.Sections {
				p.Sections[i].Actions = nil
			}
		}
		out.panels = append(out.panels, p)
	}
	return out
}

// wireWorkWith is a listed Work with what with= asked for. A nil field was
// not asked for; an asked-for field is a list, empty when there is nothing.
type wireWorkWith struct {
	wireWork
	Actions *[]wireAction `json:"actions,omitempty"`
	Panels  *[]wirePanel  `json:"panels,omitempty"`
}

// decorate adds each listed Work's actions and panels, computed as
// GET /work/{id}/actions and /panels compute them, viewWorkers at a time.
func (s *Server) decorate(ctx context.Context, items []any, ask map[string]bool, works map[string]domain.Work, scopes map[domain.ID]domain.Scope) {
	v, cancel := s.viewer(ctx)
	defer cancel()
	views := make([]targetView, len(items))
	next := make(chan int)
	var wg sync.WaitGroup
	for range viewWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				ww, _ := items[i].(wireWork)
				w := works[ww.ID]
				views[i] = v.view(extension.ActionTarget{Scope: scopes[w.ScopeID], Work: &w}, ask["actions"], ask["panels"])
			}
		}()
	}
	for i := range items {
		next <- i
	}
	close(next)
	wg.Wait()
	for i, it := range items {
		out := wireWorkWith{wireWork: it.(wireWork)}
		tv := v.prune(views[i])
		if ask["actions"] {
			out.Actions = &tv.actions
		}
		if ask["panels"] {
			out.Panels = &tv.panels
		}
		items[i] = out
	}
}

// wireEventWith is a listed Event with its routing explanation.
type wireEventWith struct {
	wireEvent
	Route *wireEventRoute `json:"route,omitempty"`
}

// ---- configuration settings ----

// wireSetting is one config field: a value its type declares safe to
// show, or Set alone for a field the type does not declare.
type wireSetting struct {
	Name  string `json:"name"`
	Value any    `json:"value,omitempty"`
	Set   bool   `json:"set,omitempty"`
}

// settings shows an opaque config object through its type's allowlist:
// the fields describe declares, with their typed values (defaults
// included), then every other top-level field by name alone. Nothing a
// type does not declare is shown, so a credential in a URL, header or
// command never is.
func settings(raw json.RawMessage, describe func(json.RawMessage) []extension.Setting) []wireSetting {
	out := []wireSetting{}
	declared := map[string]bool{}
	if describe != nil {
		for _, st := range describe(raw) {
			if declared[st.Name] {
				continue
			}
			declared[st.Name] = true
			if v, ok := settingValue(st.Value); ok {
				out = append(out, wireSetting{Name: st.Name, Value: v})
			} else {
				out = append(out, wireSetting{Name: st.Name, Set: true})
			}
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var m map[string]json.RawMessage
	if d.Decode(&m) != nil {
		return out
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if !declared[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, wireSetting{Name: k, Set: true})
	}
	return out
}

// settingValue admits the value types a Setting may carry.
func settingValue(v any) (any, bool) {
	switch x := v.(type) {
	case string, bool, int, int32, int64, uint, uint32, uint64, float32, float64:
		return x, true
	case []string:
		if x == nil {
			return []string{}, true
		}
		return x, true
	}
	return nil, false
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
