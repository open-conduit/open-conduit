package bootstrap

import (
	"context"
	"fmt"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// runEnvironment is the scheduler's Environment hook: every extension that
// contributes a run environment layer (extension.RunEnvironment) is asked
// for the attempt's run, the GitHub tools first and then the extensions in
// load order, and the layers are merged with extension.ApplyEnv. An
// in-process extension is asked only in a scope with an entry of one of its
// types; an installed one decides for itself (its manifest and the scope).
func (d *daemon) runEnvironment(ctx context.Context, state runtime.AttemptState) (map[string]string, error) {
	req := extension.RunEnvironmentRequest{Owner: extension.ResourceOwner{ScopeID: state.ScopeID, WorkID: state.WorkID, ExecutionID: state.ExecutionID}, Connection: d.configs.Connection(state)}
	type source struct {
		name string
		env  extension.RunEnvironment
	}
	var sources []source
	if d.tools != nil {
		sources = append(sources, source{"github", d.tools})
	}
	snapshot, _ := d.configs.Current()
	for _, x := range d.inproc {
		e, ok := x.(extension.RunEnvironment)
		if !ok {
			continue
		}
		if _, own := x.(*installed); !own && !scopeHasEntry(snapshot, state.ScopeID, x) {
			continue
		}
		sources = append(sources, source{x.Name(), e})
	}
	env := map[string]string{}
	for _, s := range sources {
		layer, err := s.env.RunEnvironment(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("extension %s run environment: %w", s.name, err)
		}
		extension.ApplyEnv(env, layer)
	}
	if len(env) == 0 {
		return nil, nil
	}
	return env, nil
}

// RunEnvironment asks the package for its run environment layer when the
// run's scope uses it.
func (x *installed) RunEnvironment(ctx context.Context, r extension.RunEnvironmentRequest) (map[string]string, error) {
	s, _ := x.d.extensionSnapshot()
	if s == nil || s.Scopes[r.Owner.ScopeID] == nil || !s.Scopes[r.Owner.ScopeID].Enabled || !usesExtension(s.Scopes[r.Owner.ScopeID], x.m) {
		return nil, nil
	}
	return x.r.RunEnvironment(ctx, r)
}

// scopeHasEntry reports whether the enabled scope has an entry of one of
// the extension's types.
func scopeHasEntry(s *config.Snapshot, id domain.ID, x inproc.Extension) bool {
	if s == nil || s.Scopes[id] == nil || !s.Scopes[id].Enabled {
		return false
	}
	entries, _ := typeSets(x)
	for _, e := range s.Scopes[id].Extensions {
		if entries[e.Type] {
			return true
		}
	}
	return false
}
