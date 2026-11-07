package web

import (
	"context"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/client"
)

// The rest of the fake Source: lists over the fake's Work, Events and
// configuration, and the controls the redesign added.

func (f *fake) Health(context.Context) (client.Health, error) {
	if f.health != nil {
		return *f.health, f.healthErr
	}
	return client.Health{State: "ready", Version: "0.9.0", ConfigGeneration: "4"}, f.healthErr
}
func (f *fake) Capacity(context.Context) (client.Capacity, error) {
	return client.Capacity{Pools: []client.CapacityPool{{Name: "agents", Setting: "settings.concurrency", Limit: 2, InUse: 1}, {Name: "jobs", Setting: "settings.job_concurrency", Limit: 1}}}, nil
}

func (f *fake) works(q url.Values, with bool) client.Page[client.Work] {
	states := map[string]bool{}
	for _, s := range strings.Split(q.Get("state"), ",") {
		if s != "" {
			states[s] = true
		}
	}
	var out []client.Work
	for _, w := range f.work {
		if len(states) > 0 && !states[w.State] {
			continue
		}
		w.Hold, w.Annotations = nil, nil
		if with {
			f.withCalls++
			w.Actions, w.Panels = f.actions[w.ID], f.panels[w.ID]
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return client.Page[client.Work]{Items: out}
}
func (f *fake) Works(_ context.Context, q url.Values) (client.Page[client.Work], error) {
	f.record("works "+q.Get("state"))
	if f.old && strings.Contains(q.Get("state"), ",") {
		return client.Page[client.Work]{}, &client.Error{Status: 400, Code: "invalid_filter"}
	}
	if f.old || f.noRuns {
		return f.works(q, false), nil
	}
	return f.query(q, false, false), nil
}
func (f *fake) WorksWithActions(_ context.Context, q url.Values) (client.Page[client.Work], error) {
	f.record("works+actions "+q.Get("state"))
	if f.old {
		return client.Page[client.Work]{}, &client.Error{Status: 400, Code: "invalid_filter"}
	}
	return f.works(q, true), nil
}
func (f *fake) Events(_ context.Context, q url.Values) (client.Page[client.Event], error) {
	var out []client.Event
	for _, e := range f.events {
		if q.Get("work_id") == "" || q.Get("work_id") == e.WorkID || q.Get("work_id") == e.RoutedWorkID {
			e.Route = nil
			out = append(out, e)
		}
	}
	return client.Page[client.Event]{Items: out}, nil
}
func (f *fake) EventsWithRoutes(_ context.Context, q url.Values) (client.Page[client.Event], error) {
	var out []client.Event
	for _, e := range f.events {
		if q.Get("work_id") == "" || q.Get("work_id") == e.WorkID || q.Get("work_id") == e.RoutedWorkID {
			out = append(out, e)
		}
	}
	return client.Page[client.Event]{Items: out}, nil
}
func (f *fake) TraceRoute(_ context.Context, scope string, e client.TraceEvent) (client.RouteTrace, error) {
	f.traced = append(f.traced, scope+" "+e.Type)
	if f.trace == nil {
		return client.RouteTrace{}, &client.Error{Status: 404, Code: "not_found"}
	}
	return *f.trace, nil
}
func (f *fake) RouteSamples(_ context.Context, scope string) ([]client.RouteSample, error) {
	if f.samples == nil {
		return nil, &client.Error{Status: 404, Code: "not_found"}
	}
	return f.samples[scope], nil
}
func (f *fake) RebindWork(_ context.Context, id, rev, binding, _, _ string) (client.ControlResult, error) {
	return f.control("rebind:"+binding, id, rev)
}
func (f *fake) AttachWork(_ context.Context, id, _ string) (client.AttachSpec, error) {
	return client.AttachSpec{WorkID: id, Executor: "claude-code", Argv: []string{"claude", "--resume", "sess_1"}}, nil
}
func (f *fake) ReconcileScope(_ context.Context, id, rev, _ string) (client.ReconcileResult, error) {
	f.controls = append(f.controls, "reconcile "+id+"@"+rev)
	return client.ReconcileResult{EventID: "evt_reconcile", ScopeID: id}, nil
}
func (f *fake) Wake(_ context.Context, id string, r client.WakeRequest, _ string) (client.WakeResult, error) {
	// The daemon refuses a wake event without a correlation key.
	if r.CorrelationKey == nil {
		return client.WakeResult{}, &client.Error{Status: 400, Code: "invalid_request", Message: "invalid request"}
	}
	f.controls = append(f.controls, "wake:"+r.Type+" "+id+"@"+r.ExpectedRevision+" "+r.CorrelationKey.Source+"/"+r.CorrelationKey.ExternalID)
	return client.WakeResult{Event: client.Event{ID: "evt_wake"}, Matched: true}, nil
}
func (f *fake) Doctor(context.Context, bool) (client.Doctor, error) {
	f.doctorCalled++
	if f.doctor != nil {
		return *f.doctor, nil
	}
	return client.Doctor{State: "ok"}, nil
}
func (f *fake) WorkConfigCandidate(_ context.Context, work string) (client.ConfigCandidate, error) {
	if f.candidate == nil {
		return client.ConfigCandidate{}, &client.Error{Status: 404, Code: "not_found", Message: "nothing to apply"}
	}
	return *f.candidate, nil
}
func (f *fake) Pages(context.Context) ([]client.ExtensionPage, error) { return f.pages, nil }
func (f *fake) Extensions(context.Context) ([]client.ExtensionTitle, error) {
	return f.titles, nil
}
func (f *fake) ApplyConfigText(_ context.Context, text []byte, hash, generation, _ string) (client.ConfigApplied, error) {
	if f.applyErr != nil {
		return client.ConfigApplied{}, f.applyErr
	}
	f.applied = append(f.applied, string(text)+"|"+hash+"|"+generation)
	return client.ConfigApplied{Generation: "2", Hash: hash}, nil
}

// work:
func (f *fake) EventRoute(_ context.Context, id string) (client.EventRoute, error) {
	for _, e := range f.events {
		if e.ID == id && e.Route != nil {
			return *e.Route, nil
		}
	}
	return client.EventRoute{}, &client.Error{Status: 404, Code: "not_found"}
}

// query is the daemon's GET /work filters over the fake's Work, sorted and
// paged like it, with runs attached when asked.
func (f *fake) query(q url.Values, runs, with bool) client.Page[client.Work] {
	all := f.works(q, with).Items
	var out []client.Work
	words := strings.Fields(strings.ToLower(q.Get("q")))
	for _, w := range all {
		var mine []client.Run
		for _, r := range f.runs {
			if r.WorkID == w.ID {
				mine = append(mine, r)
			}
		}
		sort.Slice(mine, func(i, j int) bool { return mine[i].Attempt < mine[j].Attempt })
		src := w.Kind
		if len(w.CorrelationKeys) > 0 {
			src = w.CorrelationKeys[0].Source
		}
		executor := ""
		if len(mine) > 0 {
			executor = mine[len(mine)-1].Executor
		}
		hay := []string{w.ID, w.Kind, w.ScopeID, w.State}
		if w.Display != nil {
			hay = append(hay, w.Display.Title, w.Display.Ref)
		}
		if n := len(mine); n > 0 && mine[n-1].Outcome != nil {
			hay = append(hay, mine[n-1].Outcome.Summary)
		}
		ok := (q.Get("scope_id") == "" || q.Get("scope_id") == w.ScopeID) && (q.Get("kind") == "" || q.Get("kind") == w.Kind) &&
			(q.Get("source") == "" || q.Get("source") == sourceFamily(src)) && (q.Get("executor") == "" || q.Get("executor") == executor) &&
			(q.Get("updated_after") == "" || w.UpdatedAt >= q.Get("updated_after"))
		text := strings.ToLower(strings.Join(hay, " "))
		for _, word := range words {
			ok = ok && strings.Contains(text, word)
		}
		if !ok {
			continue
		}
		if runs {
			w.Runs = mine
		}
		out = append(out, w)
	}
	switch q.Get("sort") {
	case "-updated":
		sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	case "updated":
		sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt < out[j].UpdatedAt })
	case "-created":
		sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	}
	total := len(out)
	from, _ := strconv.Atoi(q.Get("cursor"))
	out = out[min(from, len(out)):]
	next := ""
	if n, _ := strconv.Atoi(q.Get("limit")); n > 0 && n < len(out) {
		out, next = out[:n], strconv.Itoa(from+n)
	}
	return client.Page[client.Work]{Items: out, NextCursor: next, Total: &total}
}

func (f *fake) WorksWithRuns(_ context.Context, q url.Values, actions bool) (client.Page[client.Work], error) {
	f.record("works+runs "+q.Get("state"))
	if f.old || f.noRuns {
		return client.Page[client.Work]{}, &client.Error{Status: 400, Code: "invalid_filter"}
	}
	return f.query(q, true, actions), nil
}

func (f *fake) WorkFacets(_ context.Context, q url.Values) (client.WorkFacets, error) {
	f.record("facets "+q.Get("state"))
	if f.old || f.noRuns {
		return client.WorkFacets{}, &client.Error{Status: 400, Code: "invalid_filter"}
	}
	out := client.WorkFacets{ScopeIDs: []string{}, Sources: []string{}, Executors: []string{}}
	seen := map[string]bool{}
	for _, w := range f.query(q, true, false).Items {
		out.Total++
		src := w.Kind
		if len(w.CorrelationKeys) > 0 {
			src = w.CorrelationKeys[0].Source
		}
		add := func(list *[]string, kind, v string) {
			if v != "" && !seen[kind+v] {
				seen[kind+v] = true
				*list = append(*list, v)
			}
		}
		add(&out.ScopeIDs, "s", w.ScopeID)
		add(&out.Sources, "f", sourceFamily(src))
		if n := len(w.Runs); n > 0 {
			add(&out.Executors, "e", w.Runs[n-1].Executor)
		}
	}
	sort.Strings(out.ScopeIDs)
	sort.Strings(out.Sources)
	sort.Strings(out.Executors)
	return out, nil
}

// record notes a list call; a page makes them concurrently.
func (f *fake) record(call string) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	f.calls = append(f.calls, call)
}
