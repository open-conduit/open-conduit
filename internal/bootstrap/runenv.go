package bootstrap

import (
	"context"
	"fmt"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// runEnvironment is the scheduler's Environment hook: every package is
// asked for its run environment layer for the attempt's run, in load
// order, and the layers are merged with extension.ApplyEnv. Each package
// answers only when the run's scope uses it (RunEnvironment).
func (d *daemon) runEnvironment(ctx context.Context, state runtime.AttemptState) (map[string]string, error) {
	req := extension.RunEnvironmentRequest{Owner: extension.ResourceOwner{ScopeID: state.ScopeID, WorkID: state.WorkID, ExecutionID: state.ExecutionID}, Connection: d.configs.Connection(state)}
	env := map[string]string{}
	for _, x := range d.packages {
		layer, err := x.RunEnvironment(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("extension %s run environment: %w", x.Name(), err)
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
