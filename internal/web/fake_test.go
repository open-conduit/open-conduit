package web

import (
	"context"
	"net/url"
	"sort"
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
	f.calls = append(f.calls, "works "+q.Get("state"))
	return f.works(q, false), nil
}
func (f *fake) WorksWithActions(_ context.Context, q url.Values) (client.Page[client.Work], error) {
	f.calls = append(f.calls, "works+actions "+q.Get("state"))
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
	f.controls = append(f.controls, "wake:"+r.Type+" "+id+"@"+r.ExpectedRevision)
	return client.WakeResult{Event: client.Event{ID: "evt_wake"}, Matched: true}, nil
}
func (f *fake) Doctor(context.Context, bool) (client.Doctor, error) {
	return client.Doctor{State: "ok"}, nil
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
