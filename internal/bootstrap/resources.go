package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gitws "github.com/open-conduit/open-conduit/extensions/git"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// ResourceConfigSchema labels the opaque binding resource config handed to a
// provider.
const ResourceConfigSchema = "core.config.resource.v1"

// resourceState is the host side of the optional resource providers: the
// registered providers and the last reconcile and sweep reports for doctor.
type resourceState struct {
	providers map[string]extension.ResourceProvider
	mu        sync.Mutex
	running   bool
	last      time.Time
	report    extension.ResourceReport
	err       error
	// The spare-workspace sweep: when it last ran (or the daemon started),
	// what it found and whether it failed.
	sweeping  bool
	swept     bool
	lastSweep time.Time
	sweep     extension.ResourceSweepReport
	sweepErr  error
}

func resourceRegistry(r *config.Registry) {
	r.Resources = map[string]bool{gitws.Type: true}
	r.ResourceConfig = map[string]func(json.RawMessage) error{gitws.Type: gitws.ValidateConfig}
	r.ResourceSandbox = map[string]func(json.RawMessage, string) error{gitws.Type: gitws.ValidateSandbox}
	r.ResourceSettings = map[string]func(json.RawMessage) []extension.Setting{gitws.Type: gitws.Settings}
}

func (d *daemon) openResources() error {
	p, err := gitws.New(gitws.Options{StateDir: filepath.Join(d.o.StateDir, "resources", gitws.Type), BaseDir: filepath.Dir(d.o.ConfigPath), Now: d.now, DiskSpace: d.o.ResourceDiskSpace})
	if err != nil {
		return err
	}
	d.resources.providers = map[string]extension.ResourceProvider{gitws.Type: p}
	d.resources.lastSweep = d.now()
	return nil
}

// sweepResources runs each sweeping provider's spare-workspace sweep in the
// background, once per SweepInterval (v1: every 6 hours).
func (d *daemon) sweepResources(c *components) {
	r := &d.resources
	r.mu.Lock()
	if r.sweeping || len(r.providers) == 0 || d.now().Sub(r.lastSweep) < d.o.SweepInterval {
		r.mu.Unlock()
		return
	}
	r.sweeping = true
	r.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		var total extension.ResourceSweepReport
		var errs []error
		for _, name := range providerNames(r.providers) {
			s, ok := r.providers[name].(extension.ResourceSweeper)
			if !ok {
				continue
			}
			report, err := s.Sweep(d.ctx, func(ctx context.Context, o extension.ResourceOwner) (extension.ResourceOwnerStatus, error) {
				return d.ownerStatus(ctx, c, o)
			})
			total.Removed = append(total.Removed, report.Removed...)
			total.Kept = append(total.Kept, report.Kept...)
			if err != nil && d.ctx.Err() == nil {
				errs = append(errs, err)
			}
		}
		r.mu.Lock()
		r.sweeping, r.swept, r.lastSweep, r.sweep, r.sweepErr = false, true, d.now(), total, errors.Join(errs...)
		r.mu.Unlock()
	}()
}

// bindResources gives an attempt its binding's resource provider, bound to the
// attempt's trusted owner. A binding without a resource gets none; core never
// sees what the provider supplies.
func (d *daemon) bindResources(_ context.Context, state runtime.AttemptState) (extension.ResourceBroker, error) {
	desc, err := d.configs.Resource(state)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		return nil, nil
	}
	p := d.resources.providers[desc.Provider]
	if p == nil {
		return nil, fmt.Errorf("%w: resource provider %s", extension.ErrUnsupported, desc.Provider)
	}
	owner := extension.ResourceOwner{ScopeID: state.ScopeID, WorkID: state.WorkID, ExecutionID: state.ExecutionID}
	return extension.BindResources(p, owner, domain.Payload{Schema: ResourceConfigSchema, Data: append([]byte(nil), desc.Config...)}), nil
}

// reconcileResources releases leases whose owners finished, applies retention
// and retries failed cleanup, in the background (removal may wait for stray
// processes) and at most once per interval.
func (d *daemon) reconcileResources(c *components) {
	r := &d.resources
	r.mu.Lock()
	if r.running || len(r.providers) == 0 || d.now().Sub(r.last) < d.o.ResourceInterval {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		var total extension.ResourceReport
		var errs []error
		for _, name := range providerNames(r.providers) {
			report, err := r.providers[name].Reconcile(d.ctx, func(ctx context.Context, o extension.ResourceOwner) (extension.ResourceOwnerStatus, error) {
				return d.ownerStatus(ctx, c, o)
			})
			total.Active += report.Active
			total.Released += report.Released
			total.Removed += report.Removed
			total.Retained += report.Retained
			total.Attention = append(total.Attention, report.Attention...)
			if err != nil && d.ctx.Err() == nil {
				errs = append(errs, err)
			}
		}
		err := errors.Join(errs...)
		if err != nil {
			d.report("resources", "", "reconcile_failed")
		} else {
			d.clear("resources", "")
		}
		r.mu.Lock()
		r.running, r.last, r.report, r.err = false, d.now(), total, err
		r.mu.Unlock()
	}()
}

func providerNames(m map[string]extension.ResourceProvider) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ownerStatus reports durable state: an execution is done only when terminal
// with its physical resource exclusion confirmed, so nothing still runs in a
// workspace that is about to be removed.
func (d *daemon) ownerStatus(ctx context.Context, c *components, o extension.ResourceOwner) (extension.ResourceOwnerStatus, error) {
	var st extension.ResourceOwnerStatus
	var x domain.Execution
	err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		w, err := tx.Work(o.WorkID)
		if err != nil {
			return err
		}
		if w.ScopeID != o.ScopeID {
			return storage.ErrNotFound
		}
		st.WorkDone = w.State == domain.WorkCompleted || w.State == domain.WorkFailed || w.State == domain.WorkCancelled
		st.WorkSucceeded = w.State == domain.WorkCompleted
		x, err = tx.Execution(o.ExecutionID)
		return err
	})
	if errors.Is(err, storage.ErrNotFound) {
		return extension.ResourceOwnerStatus{}, nil
	} else if err != nil {
		return st, err
	}
	st.Known = true
	switch x.State {
	case domain.ExecutionCompleted, domain.ExecutionWaiting, domain.ExecutionHandoff, domain.ExecutionFailed, domain.ExecutionCancelled:
		state, err := c.scheduler.InspectAttempt(ctx, o.ExecutionID)
		if err != nil {
			return st, err
		}
		st.ExecutionDone = state.Excluded
		st.ExecutionSucceeded = x.State != domain.ExecutionFailed && x.State != domain.ExecutionCancelled
	}
	return st, nil
}

// resourceCheck reports leases needing attention for doctor.
func (d *daemon) resourceCheck(context.Context) runtime.Check {
	r := &d.resources
	r.mu.Lock()
	defer r.mu.Unlock()
	check := runtime.Check{Name: "resources", Status: runtime.CheckOK, Code: "ok", Detail: fmt.Sprintf("%d active resource lease(s)", r.report.Active)}
	if r.last.IsZero() {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "not_reconciled", "resource leases not yet reconciled"
		return check
	}
	for _, a := range r.report.Attention {
		item := a.Mode + " " + a.Ref + " " + a.Code
		if !a.RetainUntil.IsZero() {
			item += " until " + a.RetainUntil.UTC().Format(time.RFC3339)
		}
		check.Items = append(check.Items, item)
		if a.Code == "cleanup_failed" {
			check.Status, check.Code = runtime.CheckWarn, "cleanup_failed"
		} else if check.Status == runtime.CheckOK {
			check.Status, check.Code = runtime.CheckInfo, "retained"
		}
	}
	if r.swept {
		// The last sweep: what it removed, and the spare workspaces it kept
		// because they hold unsaved work or something runs from them.
		check.Detail += fmt.Sprintf("; the sweep at %s removed %d spare workspace(s) and kept %d", r.lastSweep.UTC().Format(time.RFC3339), len(r.sweep.Removed), len(r.sweep.Kept))
		for _, s := range r.sweep.Removed {
			check.Items = append(check.Items, "swept "+s.Mode+" "+s.Ref+" "+s.Spare)
		}
		for _, s := range r.sweep.Kept {
			check.Items = append(check.Items, "kept "+s.Mode+" "+s.Ref+" "+s.Spare+": "+s.Kept)
			if check.Status == runtime.CheckOK {
				check.Status, check.Code = runtime.CheckInfo, "spare_kept"
			}
		}
		if r.sweepErr != nil && check.Status != runtime.CheckWarn {
			check.Status, check.Code = runtime.CheckWarn, "sweep_failed"
		}
	}
	if r.err != nil {
		check.Status, check.Code = runtime.CheckWarn, "reconcile_failed"
	}
	return check
}
