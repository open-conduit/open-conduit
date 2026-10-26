package conformance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

type routeFunc func(extension.RouteInput) (extension.RouteDecision, error)

func (f routeFunc) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	return f(in)
}
func runtimeRoutes(policy string) []RuntimeRoute {
	return []RuntimeRoute{{Route: domain.Route{Name: "default", ActivePolicy: policy}, Handler: routeFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		id := domain.ID("uncorrelated/" + string(in.Event.Event.ID))
		if key := in.Event.Event.CorrelationKey; key != nil {
			id = domain.ID(key.Source + "/" + key.ExternalID)
		}
		return extension.RouteDecision{Match: true, CreateID: id, Kind: "fixture.work"}, nil
	})}}
}
func runtimeEvent(f RuntimeFixture, id string, key *domain.CorrelationKey) domain.Event {
	return domain.Event{ID: domain.ID(id), Type: "fixture.changed", Source: "fixture/account", DeliveryID: id, OccurredAt: f.Clock.Now(), ReceivedAt: f.Clock.Now(), CorrelationKey: key}
}

var runtimeKey = domain.CorrelationKey{Source: "fixture/account", ExternalID: "thread"}

const runtimeWork domain.ID = "fixture/account/thread"

func runtimeSeed(factory RuntimeFactory, o RuntimeOptions) (RuntimeFixture, domain.LeaseRef, error) {
	if o.Routes == nil {
		o.Routes = runtimeRoutes("queue")
	}
	f := factory(o)
	if err := f.Driver.Accept(runtimeEvent(f, "trigger", &runtimeKey)); err != nil {
		return f, domain.LeaseRef{}, err
	}
	if err := f.Driver.Route("trigger"); err != nil {
		return f, domain.LeaseRef{}, err
	}
	ref, err := f.Driver.Start(runtimeWork)
	return f, ref, err
}
func runtimeWait(f RuntimeFixture, ref domain.LeaseRef, resume bool, after uint64, timeout bool) error {
	w := domain.Wait{ID: "wait", AfterSequence: after, Match: domain.EventMatch{Keys: []domain.CorrelationKey{runtimeKey}, Types: []string{"fixture.changed"}}}
	if timeout {
		w.TimeoutTimerID = "timeout"
		if err := RuntimeRead(f, func(tx storage.Tx) error {
			e := runtimeEvent(f, "timeout-event", nil)
			e.Type = "core.wait.timeout"
			e.Source = "core.timer"
			e.WorkID = runtimeWork
			e.CausationID = "trigger"
			return tx.PutTimer(domain.Timer{ID: "timeout", FireAt: f.Clock.Now().Add(time.Minute), Event: e}, 0)
		}); err != nil {
			return err
		}
	}
	return f.Driver.Wait(ref, w, resume)
}
func RuntimeCases() []RuntimeCase {
	return []RuntimeCase{
		{"concurrent_correlations_and_attempts", runtimeConcurrent},
		{"independent_work_progress", runtimeIndependent},
		{"first_match_replay_and_drop_audit", runtimePolicies},
		{"multi_key_attachment_conflict_and_order", runtimeKeys},
		{"uncorrelated_and_terminal_successor", runtimeTerminal},
		{"scope_route_inheritance", runtimeScopes},
		{"unsupported_policy_explicit_fallback", runtimeFallback},
		{"wait_event_registration_orders", runtimeWaitOrders},
		{"wait_timeout_acceptance_tie", runtimeTimeout},
		{"wait_cancel_orders", runtimeWaitCancel},
		{"wait_watermark_and_timeout_only", runtimeWaitBoundary},
		{"dependency_subscription_alias", runtimeAlias},
		{"completion_inject_orders", runtimeInject},
		{"lease_loss_adoption_and_stale_result", runtimeAdoption},
		{"retry_backoff_exhaustion_and_manual_event", runtimeRetries},
		{"retry_cancel_orders", runtimeRetryCancel},
		{"causal_depth_rate_replay_and_clock", runtimeChains},
		{"concurrent_chain_budget", runtimeChainRace},
		{"unmatched_and_contradictory_target", runtimeUnmatched},
	}
}
func RunRuntime(t *testing.T, factory RuntimeFactory) {
	t.Helper()
	for _, c := range RuntimeCases() {
		t.Run(c.Name, func(t *testing.T) {
			if err := c.Check(factory); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func runtimeConcurrent(factory RuntimeFactory) error {
	f := factory(RuntimeOptions{Routes: runtimeRoutes("queue")})
	// Acceptance completes before either router starts; both refer to one key.
	for _, id := range []string{"a", "b"} {
		if err := f.Driver.Accept(runtimeEvent(f, id, &runtimeKey)); err != nil {
			return err
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	gate := make(chan struct{})
	for _, id := range []domain.ID{"a", "b"} {
		wg.Add(1)
		go func(id domain.ID) { defer wg.Done(); <-gate; errs <- f.Driver.Route(id) }(id)
	}
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, storage.ErrConflict) {
			return err
		}
	}
	// A conflict must be replayable from accepted input.
	for _, id := range []domain.ID{"a", "b"} {
		if err := f.Driver.Route(id); err != nil {
			return err
		}
	}
	refs := make(chan error, 2)
	gate = make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-gate; _, err := f.Driver.Start(runtimeWork); refs <- err }()
	}
	close(gate)
	wg.Wait()
	close(refs)
	success := 0
	for err := range refs {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		return fmt.Errorf("competing schedulers started %d attempts", success)
	}
	return RuntimeRead(f, func(tx storage.Tx) error {
		works, err := tx.ListWorks()
		if err != nil {
			return err
		}
		xs, err := tx.ListExecutions(runtimeWork)
		if err != nil {
			return err
		}
		if len(works) != 1 || len(xs) != 1 || xs[0].State != domain.ExecutionRunning {
			return fmt.Errorf("duplicate work or active attempts")
		}
		return nil
	})
}
func runtimeIndependent(factory RuntimeFactory) error {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	routes := runtimeRoutes("queue")
	base := routes[0].Handler
	routes[0].Handler = routeFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		if in.Event.Event.ID == "blocked" {
			once.Do(func() { close(entered) })
			<-release
		}
		return base.Match(context.Background(), in)
	})
	f := factory(RuntimeOptions{Routes: routes})
	other := domain.CorrelationKey{Source: "fixture/account", ExternalID: "other"}
	if err := f.Driver.Accept(runtimeEvent(f, "blocked", &runtimeKey)); err != nil {
		return err
	}
	if err := f.Driver.Accept(runtimeEvent(f, "independent", &other)); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- f.Driver.Route("blocked") }()
	defer func() { close(release); <-done }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		return fmt.Errorf("blocked handler never entered")
	}
	progress := make(chan error, 1)
	go func() {
		err := f.Driver.Route("independent")
		if err == nil {
			_, err = f.Driver.Start("fixture/account/other")
		}
		progress <- err
	}()
	select {
	case err := <-progress:
		return err
	case <-time.After(5 * time.Second):
		return fmt.Errorf("independent work globally serialized behind blocked route")
	}
}
func runtimePolicies(factory RuntimeFactory) error {
	count := 0
	routes := runtimeRoutes("drop")
	routes = append(routes, RuntimeRoute{Route: domain.Route{Name: "never"}, Handler: routeFunc(func(extension.RouteInput) (extension.RouteDecision, error) {
		count++
		return extension.RouteDecision{}, nil
	})})
	f, ref, err := runtimeSeed(factory, RuntimeOptions{Routes: routes})
	if err != nil {
		return err
	}
	if err := f.Driver.Accept(runtimeEvent(f, "dropped", &runtimeKey)); err != nil {
		return err
	}
	if err := f.Driver.Route("dropped"); err != nil {
		return err
	}
	if err := f.Driver.Route("dropped"); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("route evaluation continued after first match")
	}
	if err := f.Driver.Complete(ref); err != nil {
		return err
	}
	return RuntimeRead(f, func(tx storage.Tx) error {
		a, err := tx.Event("dropped")
		if err != nil {
			return err
		}
		w, err := tx.Work(runtimeWork)
		if err != nil {
			return err
		}
		if !a.Routed || a.Disposition != "drop" || a.RoutedWorkID != runtimeWork || a.Event.ID != "dropped" || w.State != domain.WorkCompleted {
			return fmt.Errorf("drop audit or replay changed outcome")
		}
		return nil
	})
}
func runtimeKeys(factory RuntimeFactory) error {
	second := domain.CorrelationKey{Source: runtimeKey.Source, ExternalID: "second"}
	routes := runtimeRoutes("queue")
	base := routes[0].Handler
	routes[0].Handler = routeFunc(func(in extension.RouteInput) (extension.RouteDecision, error) {
		d, err := base.Match(context.Background(), in)
		if in.Event.Event.ID == "attach" {
			d.AttachKeys = []domain.CorrelationKey{second}
		}
		return d, err
	})
	f, _, err := runtimeSeed(factory, RuntimeOptions{Routes: routes})
	if err != nil {
		return err
	}
	for _, v := range []struct {
		id  string
		key domain.CorrelationKey
	}{{"attach", runtimeKey}, {"via-second", second}} {
		if err := f.Driver.Accept(runtimeEvent(f, v.id, &v.key)); err != nil {
			return err
		}
	}
	if err := f.Driver.Route("attach"); err != nil {
		return err
	}
	if err := f.Driver.Route("via-second"); err != nil {
		return err
	}
	if err := RuntimeRead(f, func(tx storage.Tx) error {
		w, err := tx.WorkByCorrelation(second)
		if err != nil {
			return err
		}
		if w.ID != runtimeWork || len(w.CorrelationKeys) != 2 {
			return fmt.Errorf("second key split ownership")
		}
		return nil
	}); err != nil {
		return err
	}

	for _, v := range []struct {
		id  string
		key domain.CorrelationKey
	}{{"earlier", runtimeKey}, {"later", second}} {
		if err := f.Driver.Accept(runtimeEvent(f, v.id, &v.key)); err != nil {
			return err
		}
	}
	if err := f.Driver.Route("later"); !errors.Is(err, storage.ErrConflict) {
		return fmt.Errorf("multiple keys bypassed Work acceptance order: %v", err)
	}
	if err := f.Driver.Route("earlier"); err != nil {
		return err
	}
	if err := f.Driver.Route("later"); err != nil {
		return err
	}
	// A separately owned key cannot be stolen; both attachment and checkpoint roll back.
	f = factory(RuntimeOptions{Routes: routes})
	for _, v := range []struct {
		id  string
		key domain.CorrelationKey
	}{{"foreign", second}, {"attach", runtimeKey}} {
		if err := f.Driver.Accept(runtimeEvent(f, v.id, &v.key)); err != nil {
			return err
		}
	}
	if err := f.Driver.Route("foreign"); err != nil {
		return err
	}
	if err := f.Driver.Route("attach"); !errors.Is(err, storage.ErrConflict) {
		return fmt.Errorf("attachment conflict accepted: %v", err)
	}
	return RuntimeRead(f, func(tx storage.Tx) error {
		a, err := tx.Event("attach")
		if err != nil {
			return err
		}
		if a.Routed {
			return fmt.Errorf("conflicting attachment checkpoint committed")
		}
		if _, err := tx.Work(runtimeWork); !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("partial conflicting work leaked")
		}
		return nil
	})
}
func runtimeTerminal(factory RuntimeFactory) error {
	f, ref, err := runtimeSeed(factory, RuntimeOptions{})
	if err != nil {
		return err
	}
	if err := f.Driver.Complete(ref); err != nil {
		return err
	}
	for _, id := range []string{"late", "reopened"} {
		if err := f.Driver.Accept(runtimeEvent(f, id, &runtimeKey)); err != nil {
			return err
		}
		if err := f.Driver.Route(domain.ID(id)); err != nil {
			return err
		}
	}
	successor := domain.CorrelationKey{Source: runtimeKey.Source, ExternalID: "thread/generation/2"}
	if err := f.Driver.Accept(runtimeEvent(f, "successor", &successor)); err != nil {
		return err
	}
	if err := f.Driver.Route("successor"); err != nil {
		return err
	}
	if err := f.Driver.Accept(runtimeEvent(f, "unkeyed", nil)); err != nil {
		return err
	}
	if err := f.Driver.Route("unkeyed"); err != nil {
		return err
	}
	return RuntimeRead(f, func(tx storage.Tx) error {
		old, err := tx.WorkByCorrelation(runtimeKey)
		if err != nil {
			return err
		}
		next, err := tx.WorkByCorrelation(successor)
		if err != nil {
			return err
		}
		free, err := tx.Event("unkeyed")
		if err != nil {
			return err
		}
		if old.State != domain.WorkCompleted || next.ID == old.ID || free.RoutedWorkID == "" {
			return fmt.Errorf("terminal ownership or explicit uncorrelated creation broken")
		}
		return nil
	})
}
func runtimeScopes(_ RuntimeFactory) error {
	got := extension.InheritRoutes([]domain.Route{{Name: "first", Binding: "root"}, {Name: "second", Binding: "root"}}, []domain.Route{{Name: "second", Binding: "child"}, {Name: "third", Binding: "child"}})
	if len(got) != 3 || got[0].Name != "first" || got[1].Name != "second" || got[1].Binding != "child" || got[2].Name != "third" {
		return fmt.Errorf("scope override changed route slots")
	}
	return nil
}
func runtimeFallback(factory RuntimeFactory) error {
	for _, policy := range []string{"inject", "interrupt"} {
		for _, fallback := range []string{"", "queue", "drop"} {
			f, _, err := runtimeSeed(factory, RuntimeOptions{Routes: runtimeRoutes(policy), Fallback: fallback})
			// Implementations may reject the invalid binding before any route executes.
			if err != nil {
				if fallback == "" && errors.Is(err, extension.ErrUnsupported) {
					continue
				}
				return err
			}
			if err := f.Driver.Accept(runtimeEvent(f, "active", &runtimeKey)); err != nil {
				return err
			}
			err = f.Driver.Route("active")
			if fallback == "" {
				if !errors.Is(err, extension.ErrUnsupported) {
					return fmt.Errorf("unsupported %s silently fell back: %v", policy, err)
				}
				continue
			}
			if err != nil {
				return err
			}
			if err := RuntimeRead(f, func(tx storage.Tx) error {
				a, err := tx.Event("active")
				if err == nil && a.Disposition != fallback {
					return fmt.Errorf("explicit fallback not recorded")
				}
				return err
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
func runtimeWaitOrders(factory RuntimeFactory) error {
	for _, resume := range []bool{false, true} {
		for _, before := range []bool{false, true} {
			f, ref, err := runtimeSeed(factory, RuntimeOptions{Capabilities: domain.Capabilities{Resume: resume}})
			if err != nil {
				return err
			}
			if before {
				if err := f.Driver.Accept(runtimeEvent(f, "wake", &runtimeKey)); err != nil {
					return err
				}
			}
			if err := runtimeWait(f, ref, resume, 1, false); err != nil {
				return err
			}
			if !before {
				if err := f.Driver.Accept(runtimeEvent(f, "wake", &runtimeKey)); err != nil {
					return err
				}
			}
			if err := f.Driver.Wake("wait"); err != nil {
				return err
			}
			if err := f.Driver.Wake("wait"); err != nil {
				return err
			}
			next, err := f.Driver.Start(runtimeWork)
			if err != nil {
				return err
			}
			if resume && (next.ExecutionID != ref.ExecutionID || next.Epoch <= ref.Epoch) || !resume && next.ExecutionID == ref.ExecutionID {
				return fmt.Errorf("wake bypassed resume/attempt N+1 path")
			}
			if err := RuntimeRead(f, func(tx storage.Tx) error {
				w, err := tx.Wait("wait")
				if err != nil {
					return err
				}
				xs, err := tx.ListExecutions(runtimeWork)
				if err != nil {
					return err
				}
				want := 1
				if !resume {
					want = 2
				}
				if w.State != domain.WaitWoken || w.WakeEventID != "wake" || len(xs) != want {
					return fmt.Errorf("wait woke more than once or lost accepted event")
				}
				return nil
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
func runtimeTimeout(factory RuntimeFactory) error {
	for _, eventFirst := range []bool{false, true} {
		f, ref, err := runtimeSeed(factory, RuntimeOptions{})
		if err != nil {
			return err
		}
		if err := runtimeWait(f, ref, false, 1, true); err != nil {
			return err
		}
		f.Clock.Advance(time.Minute)
		if eventFirst {
			if err := f.Driver.Accept(runtimeEvent(f, "wake", &runtimeKey)); err != nil {
				return err
			}
		} else {
			if err := RuntimeRead(f, func(tx storage.Tx) error {
				t, err := tx.Timer("timeout")
				if err != nil {
					return err
				}
				return tx.FireTimer(t.ID, t.Revision, nil)
			}); err != nil {
				return err
			}
			if err := f.Driver.Accept(runtimeEvent(f, "wake", &runtimeKey)); err != nil {
				return err
			}
		}
		if err := f.Driver.Wake("wait"); err != nil {
			return err
		}
		if err := f.Driver.Wake("wait"); err != nil {
			return err
		}
		if err := RuntimeRead(f, func(tx storage.Tx) error {
			w, err := tx.Wait("wait")
			if err != nil {
				return err
			}
			want := domain.ID("timeout-event")
			if eventFirst {
				want = "wake"
			}
			if w.WakeEventID != want {
				return fmt.Errorf("timeout tie ignored acceptance order: got %s want %s", w.WakeEventID, want)
			}
			if _, err := tx.Timer("timeout"); !errors.Is(err, storage.ErrNotFound) {
				return fmt.Errorf("timeout survived wake")
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
func runtimeWaitCancel(factory RuntimeFactory) error {
	for _, cancelFirst := range []bool{false, true} {
		f, ref, err := runtimeSeed(factory, RuntimeOptions{})
		if err != nil {
			return err
		}
		if err := runtimeWait(f, ref, false, 1, true); err != nil {
			return err
		}
		if err := f.Driver.Accept(runtimeEvent(f, "wake", &runtimeKey)); err != nil {
			return err
		}
		if cancelFirst {
			if err := f.Driver.Cancel(runtimeWork); err != nil {
				return err
			}
		}
		if err := f.Driver.Wake("wait"); err != nil {
			return err
		}
		if err := f.Driver.Cancel(runtimeWork); err != nil {
			return err
		}
		if _, err := f.Driver.Start(runtimeWork); err == nil {
			return fmt.Errorf("cancelled wait started execution")
		}
		if err := RuntimeRead(f, func(tx storage.Tx) error {
			w, err := tx.Wait("wait")
			if err != nil {
				return err
			}
			want := domain.WaitWoken
			if cancelFirst {
				want = domain.WaitCancelled
			}
			if w.State != want {
				return fmt.Errorf("cancel overwrote committed wait winner")
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
func runtimeAlias(factory RuntimeFactory) error {
	f, ref, err := runtimeSeed(factory, RuntimeOptions{})
	if err != nil {
		return err
	}
	foreign := domain.CorrelationKey{Source: runtimeKey.Source, ExternalID: "dependency"}
	if err := f.Driver.Accept(runtimeEvent(f, "dependency", &foreign)); err != nil {
		return err
	}
	if err := f.Driver.Route("dependency"); err != nil {
		return err
	}
	// Foreign key is not legal on the dependent Work's Wait.
	bad := domain.Wait{ID: "foreign-wait", AfterSequence: 1, Match: domain.EventMatch{Keys: []domain.CorrelationKey{foreign}}}
	if err := f.Driver.Wait(ref, bad, false); !errors.Is(err, storage.ErrInvalid) {
		return fmt.Errorf("foreign wait stole dependency ownership: %v", err)
	}
	if err := runtimeWait(f, ref, false, 1, false); err != nil {
		return err
	}
	alias := runtimeEvent(f, "subscription/wait/dependency", &runtimeKey)
	alias.CausationID = "dependency"
	alias.WorkID = runtimeWork
	if err := f.Driver.Derive(alias); err != nil {
		return err
	}
	if err := f.Driver.Derive(alias); err != nil {
		return err
	}
	if err := f.Driver.Wake("wait"); err != nil {
		return err
	}
	return RuntimeRead(f, func(tx storage.Tx) error {
		owner, err := tx.WorkByCorrelation(foreign)
		if err != nil {
			return err
		}
		w, err := tx.Wait("wait")
		if err != nil {
			return err
		}
		if owner.ID == runtimeWork || w.WakeEventID != alias.ID {
			return fmt.Errorf("subscription alias lost correlation/causation")
		}
		return nil
	})
}
func runtimeInject(factory RuntimeFactory) error {
	for _, completionFirst := range []bool{false, true} {
		f, ref, err := runtimeSeed(factory, RuntimeOptions{Routes: runtimeRoutes("inject"), Capabilities: domain.Capabilities{Inject: true}})
		if err != nil {
			return err
		}
		if completionFirst {
			if err := f.Driver.Complete(ref); err != nil {
				return err
			}
		}
		if err := f.Driver.Accept(runtimeEvent(f, "incoming", &runtimeKey)); err != nil {
			return err
		}
		if !completionFirst {
			if err := f.Driver.Complete(ref); !errors.Is(err, storage.ErrConflict) {
				return fmt.Errorf("completion overtook accepted unprocessed event: %v", err)
			}
		}
		if err := f.Driver.Route("incoming"); err != nil {
			return err
		}
		if !completionFirst {
			if err := f.Driver.Complete(ref); err != nil {
				return err
			}
			if _, err := f.Driver.Start(runtimeWork); err != nil {
				return fmt.Errorf("unconfirmed injection lost at completion: %w", err)
			}
		}
		if err := RuntimeRead(f, func(tx storage.Tx) error {
			a, err := tx.Event("incoming")
			if err != nil {
				return err
			}
			want := "inject"
			if completionFirst {
				want = "terminal"
			}
			if a.Disposition != want {
				return fmt.Errorf("completion/inject winner not checkpointed")
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
func runtimeAdoption(factory RuntimeFactory) error {
	for _, adoptable := range []bool{false, true} {
		f, old, err := runtimeSeed(factory, RuntimeOptions{})
		if err != nil {
			return err
		}
		if _, err := f.Driver.Recover(old.ExecutionID, adoptable); err == nil {
			return fmt.Errorf("live lease was adopted")
		}
		f.Clock.Advance(RuntimeLease)
		ref, err := f.Driver.Recover(old.ExecutionID, adoptable)
		if err != nil {
			return err
		}
		if ref.ExecutionID != old.ExecutionID || ref.Epoch <= old.Epoch {
			return fmt.Errorf("adoption started a replacement or reused epoch")
		}
		if err := f.Driver.Complete(old); !errors.Is(err, storage.ErrLeaseLost) {
			return fmt.Errorf("stale completion accepted: %v", err)
		}
		if err := RuntimeRead(f, func(tx storage.Tx) error {
			xs, err := tx.ListExecutions(runtimeWork)
			if err != nil {
				return err
			}
			want := domain.ExecutionFailed
			if adoptable {
				want = domain.ExecutionRunning
			}
			if len(xs) != 1 || xs[0].State != want {
				return fmt.Errorf("unverifiable adoption launched a replacement")
			}
			return nil
		}); err != nil {
			return err
		}
		if err := f.Driver.Cancel(runtimeWork); err != nil {
			return err
		}
		if _, err := f.Driver.Recover(old.ExecutionID, true); err == nil {
			return fmt.Errorf("cancelled execution adopted")
		}
	}
	return nil
}
func runtimeRetries(factory RuntimeFactory) error {
	f, ref, err := runtimeSeed(factory, RuntimeOptions{MaxAttempts: 4})
	if err != nil {
		return err
	}
	for n := 1; n <= 4; n++ {
		if err := f.Driver.Fail(ref); err != nil {
			return err
		}
		if _, err := f.Driver.Start(runtimeWork); err == nil {
			return fmt.Errorf("failure bypassed eligibility")
		}
		if n == 4 {
			break
		}
		if err := f.Driver.Retry(runtimeWork, "", false); err == nil {
			return fmt.Errorf("retry ran before backoff")
		}
		f.Clock.Advance(time.Duration(n) * 5 * time.Minute)
		if err := f.Driver.Retry(runtimeWork, "", false); err != nil {
			return err
		}
		ref, err = f.Driver.Start(runtimeWork)
		if err != nil {
			return err
		}
	}
	if err := f.Driver.Retry(runtimeWork, "", false); err == nil {
		return fmt.Errorf("exhausted automatic retries continued")
	}
	manual := runtimeEvent(f, "manual", nil)
	manual.Type = "core.retry.requested"
	manual.WorkID = runtimeWork
	if err := f.Driver.Accept(manual); err != nil {
		return err
	}
	if err := f.Driver.Retry(runtimeWork, manual.ID, true); err != nil {
		return err
	}
	ref, err = f.Driver.Start(runtimeWork)
	if err != nil {
		return err
	}
	if err := f.Driver.Fail(ref); err != nil {
		return err
	}
	if err := f.Driver.Retry(runtimeWork, manual.ID, true); err != nil {
		return err
	}
	if _, err := f.Driver.Start(runtimeWork); err == nil {
		return fmt.Errorf("replayed manual event cleared newer backoff")
	}
	if err := RuntimeRead(f, func(tx storage.Tx) error {
		timer, err := tx.Timer(domain.ID("retry/" + string(runtimeWork)))
		if err != nil {
			return err
		}
		if !timer.FireAt.Equal(f.Clock.Now().Add(5 * time.Minute)) {
			return fmt.Errorf("manual retry failed to reset failure budget")
		}
		return nil
	}); err != nil {
		return err
	}
	return RuntimeRead(f, func(tx storage.Tx) error {
		xs, err := tx.ListExecutions(runtimeWork)
		if err != nil {
			return err
		}
		if len(xs) != 5 {
			return fmt.Errorf("manual retry did not create N+1")
		}
		for _, x := range xs {
			if x.Attempt == 5 && x.TriggerID != manual.ID {
				return fmt.Errorf("manual retry bypassed accepted event")
			}
		}
		return nil
	})
}
func runtimeRetryCancel(factory RuntimeFactory) error {
	for _, cancelFirst := range []bool{false, true} {
		f, ref, err := runtimeSeed(factory, RuntimeOptions{})
		if err != nil {
			return err
		}
		if err := f.Driver.Fail(ref); err != nil {
			return err
		}
		f.Clock.Advance(5 * time.Minute)
		if cancelFirst {
			if err := f.Driver.Cancel(runtimeWork); err != nil {
				return err
			}
			if err := f.Driver.Retry(runtimeWork, "", false); err == nil {
				return fmt.Errorf("cancelled work retried")
			}
		} else {
			if err := f.Driver.Retry(runtimeWork, "", false); err != nil {
				return err
			}
			if err := f.Driver.Cancel(runtimeWork); err != nil {
				return err
			}
		}
		if _, err := f.Driver.Start(runtimeWork); err == nil {
			return fmt.Errorf("retry/cancel left runnable work")
		}
	}
	return nil
}
func runtimeChains(factory RuntimeFactory) error {
	f := factory(RuntimeOptions{MaxDepth: 2, PerMinute: 2})
	root := runtimeEvent(f, "root", nil)
	if err := f.Driver.Accept(root); err != nil {
		return err
	}
	parent := root.ID
	for _, id := range []string{"child-created", "generated-timer"} {
		e := runtimeEvent(f, id, nil)
		e.Type = "core.work.created"
		if id == "generated-timer" {
			e.Type = "core.timer.fired"
		}
		e.CausationID = parent
		if err := f.Driver.Derive(e); err != nil {
			return err
		}
		if err := f.Driver.Derive(e); err != nil {
			return fmt.Errorf("replay consumed chain budget: %w", err)
		}
		parent = e.ID
	}
	if f.Restart == nil {
		return fmt.Errorf("missing restart adapter")
	}
	f.Driver = f.Restart()
	tooDeep := runtimeEvent(f, "too-deep", nil)
	tooDeep.CausationID = parent
	if err := f.Driver.Derive(tooDeep); err == nil {
		return fmt.Errorf("chain depth bound bypassed")
	}
	tooFast := runtimeEvent(f, "too-fast", nil)
	tooFast.CausationID = root.ID
	if err := f.Driver.Derive(tooFast); err == nil {
		return fmt.Errorf("per-chain rate bound bypassed")
	}
	f.Clock.Advance(time.Minute)
	if err := f.Driver.Derive(tooFast); err != nil {
		return err
	}
	if err := f.Driver.Derive(tooDeep); err == nil {
		return fmt.Errorf("clock reset chain depth")
	}
	return RuntimeRead(f, func(tx storage.Tx) error {
		if _, err := tx.Event("too-deep"); !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("rejected child leaked")
		}
		return nil
	})
}

func runtimeWaitBoundary(factory RuntimeFactory) error {
	f, ref, err := runtimeSeed(factory, RuntimeOptions{})
	if err != nil {
		return fmt.Errorf("boundary: %w", err)
	}
	// The trigger at sequence 1 must not satisfy an exclusive boundary of 1.
	if err := runtimeWait(f, ref, false, 1, true); err != nil {
		return fmt.Errorf("boundary: %w", err)
	}
	if err := RuntimeRead(f, func(tx storage.Tx) error {
		w, err := tx.Wait("wait")
		if err == nil && w.State != domain.WaitActive {
			return fmt.Errorf("watermark reused observed event")
		}
		return err
	}); err != nil {
		return fmt.Errorf("boundary: %w", err)
	}
	f.Driver = f.Restart()
	f.Clock.Advance(time.Minute)
	if err := f.Driver.Wake("wait"); err != nil {
		return fmt.Errorf("boundary wake: %w", err)
	}
	if err := RuntimeRead(f, func(tx storage.Tx) error {
		w, err := tx.Wait("wait")
		if err == nil && w.WakeEventID != "timeout-event" {
			return fmt.Errorf("deadline woke without accepted timeout")
		}
		return err
	}); err != nil {
		return fmt.Errorf("boundary: %w", err)
	}
	if _, err := f.Driver.Start(runtimeWork); err != nil {
		return fmt.Errorf("boundary start: %w", err)
	}
	return nil
}
func runtimeChainRace(factory RuntimeFactory) error {
	f := factory(RuntimeOptions{PerMinute: 1})
	if err := f.Driver.Accept(runtimeEvent(f, "root", nil)); err != nil {
		return err
	}
	gate := make(chan struct{})
	results := make(chan error, 2)
	for _, id := range []string{"left", "right"} {
		go func(id string) {
			<-gate
			e := runtimeEvent(f, id, nil)
			e.CausationID = "root"
			results <- f.Driver.Derive(e)
		}(id)
	}
	close(gate)
	success := 0
	for i := 0; i < 2; i++ {
		if <-results == nil {
			success++
		}
	}
	if success != 1 {
		return fmt.Errorf("concurrent siblings exceeded shared chain budget: %d", success)
	}
	return RuntimeRead(f, func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		if err != nil {
			return err
		}
		if len(all) != 2 {
			return fmt.Errorf("chain rejection leaked accepted effects")
		}
		return nil
	})
}
func runtimeUnmatched(factory RuntimeFactory) error {
	f := factory(RuntimeOptions{})
	if err := f.Driver.Accept(runtimeEvent(f, "unmatched", nil)); err != nil {
		return err
	}
	if err := f.Driver.Route("unmatched"); err != nil {
		return err
	}
	if err := RuntimeRead(f, func(tx storage.Tx) error {
		a, err := tx.Event("unmatched")
		if err != nil {
			return err
		}
		if !a.Routed || a.Disposition != "unmatched" || a.RoutedWorkID != "" {
			return fmt.Errorf("unmatched audit missing")
		}
		return nil
	}); err != nil {
		return err
	}
	f, _, err := runtimeSeed(factory, RuntimeOptions{})
	if err != nil {
		return err
	}
	key := domain.CorrelationKey{Source: runtimeKey.Source, ExternalID: "unowned"}
	e := runtimeEvent(f, "contradictory", &key)
	e.WorkID = runtimeWork
	if err := f.Driver.Accept(e); err != nil {
		return err
	}
	if err := f.Driver.Route(e.ID); !errors.Is(err, storage.ErrInvalid) {
		return fmt.Errorf("explicit work bypassed correlation authority: %v", err)
	}
	return nil
}
