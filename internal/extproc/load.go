package extproc

import "context"

// Load starts a discovered extension behind its scoped service and returns
// the adapter. Options.Found and Options.Service are set from f and deps.
func Load(ctx context.Context, f Found, deps Deps, o Options) (*Remote, error) {
	svc, err := NewService(f.Manifest, deps)
	if err != nil {
		return nil, err
	}
	o.Found, o.Service = f, svc
	p, err := Start(ctx, o)
	if err != nil {
		return nil, err
	}
	return NewRemote(p, svc), nil
}
