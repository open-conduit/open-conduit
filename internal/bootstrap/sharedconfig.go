package bootstrap

import (
	"context"

	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/storage"
)

// refuseUnservableConfig refuses, before Open changes anything, shared state
// whose stored generation needs something this host doesn't have, such as a
// package installed only on the other hosts. It validates against the installed
// packages' manifests, so it starts no process; OpenHeldConfig has the last word
// under the lease, with the packages' own validators.
//
// A standby asks on every retry, so a generation that passed isn't checked
// again: only a new one costs a walk of the installed packages.
func (d *daemon) refuseUnservableConfig(ctx context.Context) error {
	var generation uint64
	var normalized []byte
	if err := viewSharedState(ctx, d.o.StateDir, func(tx storage.Tx) error {
		var err error
		generation, normalized, err = management.StoredGenerationIn(tx)
		return err
	}); err != nil {
		return err
	}
	if generation == 0 || generation == d.servableConfig || generation == d.o.AdoptConfig {
		return nil
	}
	found, _ := discoverUnder(d.o.ConfigPath, "", documentPolicy(normalized).withRoot(d.policy), nil)
	xs := make([]*installed, 0, len(found))
	for _, f := range found {
		xs = append(xs, &installed{m: f.Manifest, dir: f.Dir})
	}
	reg := manifestRegistry(xs)
	reg.Policy = d.policy
	if _, err := config.ValidateStored(normalized, reg); err != nil {
		return management.RefuseStoredConfig(generation, normalized, err)
	}
	d.servableConfig = generation
	return nil
}
