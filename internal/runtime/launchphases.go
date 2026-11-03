package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// PhaseFunc begins a launch phase named name; the function it returns ends
// it, with the outcome err gives (nil: ok). Ending it twice is harmless.
type PhaseFunc func(name string) (end func(err error))

type phaseKey struct{}

// LaunchPhases is the launch-phase recorder of the launch ctx belongs to,
// for the hooks a launch calls (Resources, Sandbox, Container) to record
// the phases they run later, such as a container's plan and creation. A
// context outside a launch gets one that records nothing.
func LaunchPhases(ctx context.Context) PhaseFunc {
	if f, ok := ctx.Value(phaseKey{}).(PhaseFunc); ok {
		return f
	}
	return func(string) func(error) { return func(error) {} }
}

// launchTrail records a launch's phases with the attempt's open segment,
// each fenced by the launch's lease: a phase is written as it begins, so a
// slow one shows while it runs, and again as it ends. A write that fails
// (the lease was lost) only leaves the trail short; it never fails the
// launch. Once the launch returns, no new phase begins.
type launchTrail struct {
	s      *Scheduler
	ctx    context.Context
	ref    domain.LeaseRef
	closed atomic.Bool
}

func (t *launchTrail) begin(name string) func(error) {
	if t.closed.Load() {
		return func(error) {}
	}
	index, segment := -1, 0
	err := t.s.transact(t.ctx, &t.ref, func(tx storage.Tx) error {
		index = -1
		state, e := attemptState(tx, t.ref.ExecutionID)
		if e != nil {
			return e
		}
		n := len(state.Segments)
		if n == 0 || state.Segments[n-1].EndedAt != nil || len(state.Segments[n-1].Launch) >= MaxLaunchPhases {
			return nil
		}
		g := &state.Segments[n-1]
		g.Launch = append(g.Launch, LaunchPhase{Name: name, StartedAt: t.s.now()})
		index, segment = len(g.Launch)-1, state.SegmentNumber()
		return putAttemptState(tx, state)
	})
	if err != nil {
		index = -1
	}
	var once sync.Once
	return func(cause error) {
		once.Do(func() {
			if index < 0 {
				return
			}
			outcome := "ok"
			switch {
			case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded), cause != nil && t.ctx.Err() != nil:
				outcome = "cancelled"
			case cause != nil:
				outcome = "failed"
			}
			_ = t.s.transact(context.WithoutCancel(t.ctx), &t.ref, func(tx storage.Tx) error {
				state, e := attemptState(tx, t.ref.ExecutionID)
				if e != nil {
					return e
				}
				n := len(state.Segments)
				if n == 0 || state.SegmentNumber() != segment || index >= len(state.Segments[n-1].Launch) {
					return nil
				}
				p := &state.Segments[n-1].Launch[index]
				if p.Name != name || p.EndedAt != nil {
					return nil
				}
				end := t.s.now()
				p.EndedAt, p.Outcome = &end, outcome
				return putAttemptState(tx, state)
			})
		})
	}
}

// phase runs fn as the phase name.
func (t *launchTrail) phase(name string, fn func() error) error {
	end := t.begin(name)
	err := fn()
	end(err)
	return err
}
