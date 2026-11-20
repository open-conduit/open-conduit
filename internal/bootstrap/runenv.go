package bootstrap

import (
	"context"
	"fmt"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// runEnvironment is the scheduler's Environment hook: every package is
// asked for its run environment layer for the attempt's run, in load
// order, and the layers are merged with extension.ApplyEnv. Each package
// answers only when the run's scope uses it (RunEnvironment). The files a
// layer carries are written into the package's own directory of the run's
// files (management.Endpoints.FilesDir) before the run starts; the
// scheduler has served the execution's endpoint by then, so the token
// directory they go in exists.
func (d *daemon) runEnvironment(ctx context.Context, state runtime.AttemptState) (map[string]string, error) {
	req := extension.RunEnvironmentRequest{Owner: extension.ResourceOwner{ScopeID: state.ScopeID, WorkID: state.WorkID, ExecutionID: state.ExecutionID}, Connection: d.configs.Connection(state), Binding: state.Binding.Name}
	env := map[string]string{}
	for _, x := range d.extensionPackages() {
		req.FilesDir = ""
		if d.endpoints != nil {
			req.FilesDir = d.endpoints.FilesDir(state.ExecutionID, x.Name())
		}
		layer, err := x.RunLayer(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("extension %s run environment: %w", x.Name(), err)
		}
		if req.FilesDir != "" {
			if err := management.WriteFiles(req.FilesDir, layer.Files); err != nil {
				return nil, fmt.Errorf("extension %s run files: %w", x.Name(), err)
			}
		}
		extension.ApplyEnv(env, layer.Env)
	}
	if len(env) == 0 {
		return nil, nil
	}
	return env, nil
}

// RunEnvironment asks the package for its run environment layer when the
// run's scope uses it.
func (x *installed) RunEnvironment(ctx context.Context, r extension.RunEnvironmentRequest) (map[string]string, error) {
	layer, err := x.RunLayer(ctx, r)
	return layer.Env, err
}

// RunLayer is RunEnvironment with the layer's files.
func (x *installed) RunLayer(ctx context.Context, r extension.RunEnvironmentRequest) (extension.RunLayer, error) {
	s, _ := x.d.extensionSnapshot()
	if s == nil || s.Scopes[r.Owner.ScopeID] == nil || !s.Scopes[r.Owner.ScopeID].Enabled || !usesExtension(s.Scopes[r.Owner.ScopeID], x.m) {
		return extension.RunLayer{}, nil
	}
	return x.r.RunLayer(ctx, r)
}
