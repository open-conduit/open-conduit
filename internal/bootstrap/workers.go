package bootstrap

import (
	"context"
	"errors"
	"sync"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// The phases of the host's pass that wait on another process run on
// workers of their own, so a slow provider or a wedged executor package
// holds up neither routing, timers and starts nor each other (#701). The
// pass keeps what only reads and writes storage, since that shares the one
// writer connection whatever runs it. Ordering per Work needs nothing from
// the pass's sequence: claims, routes and completions are serialisable
// transactions with revision checks, an ingress source's scan holds its own
// lease, and a control intent's dispatch holds its own claim.

// ingressWorkers bounds the provider pages read at once, one per source.
const ingressWorkers = 8

// controlWorkers bounds the Work whose control intents are dispatched at
// once, one worker per Work, which delivers that Work's intents in order.
const controlWorkers = 16

// workerState records the sources and Work a worker is busy with, so a
// pass hands each to at most one worker at a time.
type workerState struct {
	mu       sync.Mutex
	ingress  map[string]bool
	controls map[domain.ID]bool
}

// take marks key busy in busy and reports whether it was free and the
// busy set had room under limit.
func take[K comparable](mu *sync.Mutex, busy *map[K]bool, key K, limit int) bool {
	mu.Lock()
	defer mu.Unlock()
	if *busy == nil {
		*busy = map[K]bool{}
	}
	if (*busy)[key] || len(*busy) >= limit {
		return false
	}
	(*busy)[key] = true
	return true
}

func release[K comparable](mu *sync.Mutex, busy map[K]bool, key K) {
	mu.Lock()
	delete(busy, key)
	mu.Unlock()
}

// reconcileIngress hands each due ingress source to a worker of its own,
// at most ingressWorkers at once. A worker reads one provider page under
// the source's scan lease (Ingress.ReconcileSource, bounded by its page
// timeout) and nudges the pass when the page accepted Events, so they are
// routed at once. A source still being read is left for a later pass.
// Each worker uses the daemon's one Ingress, whose registrations a reload
// replaces in place, so the next page a worker reads is the new source's.
func (d *daemon) reconcileIngress(ctx context.Context) {
	if d.ingress == nil {
		return
	}
	names, err := d.ingress.DueSources(ctx)
	if err != nil {
		if ctx.Err() == nil {
			d.report("ingress", "", errorCode(err))
		}
		return
	}
	d.clear("ingress", "")
	w := &d.workers
	for _, name := range names {
		if !take(&w.mu, &w.ingress, name, ingressWorkers) {
			continue
		}
		d.goSafe("ingress", func() {
			defer release(&w.mu, w.ingress, name)
			result, err := d.ingress.ReconcileSource(ctx, name)
			if errors.Is(err, runtime.ErrIngressBusy) {
				return
			}
			if err != nil {
				if ctx.Err() == nil {
					d.report("ingress", domain.ID(name), errorCode(err))
				}
				return
			}
			d.clear("ingress", domain.ID(name))
			if len(result.Accepted) > 0 {
				d.poke()
			}
		})
	}
}

// dispatchControls hands each running Work with control intents to a
// worker of its own, at most controlWorkers at once, which dispatches the
// Work's intents in order (Scheduler.DispatchControl). Each call to the
// executor has a deadline inside its dispatch claim, so a hung executor
// holds up its own Work's controls and nothing else, and a call that runs
// out of time is retried as ambiguous on a later pass. A worker nudges no
// pass: what a dispatch commits is the intent's own state, and the end of
// an interrupted attempt reaches the pass through its holder.
func (d *daemon) dispatchControls(ctx context.Context, c *components) {
	running := map[domain.ID]bool{}
	err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		clear(running)
		works, err := tx.LiveWorks()
		for _, w := range works {
			if w.State == domain.WorkRunning {
				running[w.ID] = true
			}
		}
		return err
	})
	if err != nil || len(running) == 0 {
		return
	}
	// One read of the intents for every running Work, in their order.
	all, err := c.router.Intents(ctx, "")
	if err != nil {
		return
	}
	byWork := map[domain.ID][]string{}
	var order []domain.ID
	for _, intent := range all {
		if !running[intent.WorkID] {
			continue
		}
		if _, ok := byWork[intent.WorkID]; !ok {
			order = append(order, intent.WorkID)
		}
		byWork[intent.WorkID] = append(byWork[intent.WorkID], intent.ID)
	}
	w := &d.workers
	for _, work := range order {
		if !take(&w.mu, &w.controls, work, controlWorkers) {
			continue
		}
		intents := byWork[work]
		d.goSafe("control", func() {
			defer release(&w.mu, w.controls, work)
			// The generation current when the worker starts, as an
			// attendance pins its own.
			scheduler := d.current.Load().scheduler
			for _, id := range intents {
				if ctx.Err() != nil {
					return
				}
				if err := scheduler.DispatchControl(ctx, id); err != nil {
					d.report("control", domain.ID(id), errorCode(err))
				}
			}
		})
	}
}
