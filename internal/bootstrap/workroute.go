package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
)

// workRoute is the first-party "work" route handler: it binds child Work,
// which another Work's execution asked for with create_work, to the route's
// binding. It takes only the child's own creation (a core.work.created Event
// from core.work for Work with a parent) whose kind its config lists:
//
//	{"handler": "work", "binding": "engineer", "config": {"kinds": [
//		{"kind": "agent.task.engineer", "description": "The engineering agent: code, builds, deploys and incidents."}]}}
//
// The operator decides in config which kinds exist, which binding does
// each and what each is for (the description an asking agent is shown);
// who may ask for a kind is the asking binding's work_kinds, checked by the
// host when it serves create_work.
type workRoute struct{}

// workRouteConfig is a work route's config.
type workRouteConfig struct {
	Kinds []workRouteKind `json:"kinds"`
}

// workRouteKind is one kind a work route takes, and what it is for.
type workRouteKind struct {
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
}

// takes reports whether the route takes child Work of kind.
func (c workRouteConfig) takes(kind string) (workRouteKind, bool) {
	for _, k := range c.Kinds {
		if k.Kind == kind {
			return k, true
		}
	}
	return workRouteKind{}, false
}

// describeWorkKind is what kind is for in scope: the description of the
// first enabled work route that takes it, or "".
func describeWorkKind(scope config.EffectiveScope, kind string) string {
	for _, r := range scope.Routes {
		if r.Handler != "work" {
			continue
		}
		if c, err := parseWorkRoute(r.Config); err == nil {
			if k, ok := c.takes(kind); ok && k.Description != "" {
				return k.Description
			}
		}
	}
	return ""
}

// parseWorkRoute decodes and checks a work route's config: one or more
// kinds, each a namespaced Work kind, none repeated.
func parseWorkRoute(raw json.RawMessage) (workRouteConfig, error) {
	var c workRouteConfig
	if len(raw) == 0 {
		return c, errors.New(`a work route's config lists the kinds of child Work it takes: {"kinds": [{"kind": "agent.task.engineer", "description": "…"}]}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("a work route's config is {\"kinds\": [{\"kind\": ..., \"description\": ...}]}: %w", err)
	}
	if len(c.Kinds) == 0 {
		return c, errors.New("a work route's config lists at least one kind")
	}
	for i, k := range c.Kinds {
		if !config.WorkKind(k.Kind) {
			return c, fmt.Errorf("kind %q is not a Work kind: lower-case words joined by dots, such as agent.task.engineer", k.Kind)
		}
		if _, ok := (workRouteConfig{Kinds: c.Kinds[:i]}).takes(k.Kind); ok {
			return c, fmt.Errorf("kind %q is listed twice", k.Kind)
		}
	}
	return c, nil
}

func (workRoute) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if e.Type != "core.work.created" || e.Source != "core.work" {
		return extension.Skip("not the creation of child Work"), nil
	}
	if in.Work == nil || in.Work.ID != e.WorkID || in.Work.ParentID == "" {
		return extension.Skip("the Work it creates has no parent"), nil
	}
	c, err := parseWorkRoute(in.Route.Config.Data)
	if err != nil {
		return extension.Skip(err.Error()), nil
	}
	if _, ok := c.takes(in.Work.Kind); !ok {
		return extension.Skip("child Work of kind " + in.Work.Kind + ", which this route does not take"), nil
	}
	return extension.RouteDecision{Match: true, Reason: "child Work of kind " + in.Work.Kind}, nil
}
