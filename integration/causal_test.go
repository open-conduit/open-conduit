package integration_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

func causalEvent(id string) domain.Event {
	return domain.Event{ID: domain.ID(id), Type: "fixture.derived", Source: "fixture.causal", DeliveryID: id, OccurredAt: ingressTime, ReceivedAt: ingressTime}
}
func causalFixture(t *testing.T, limits runtime.CausalLimits) (*sqlite.Store, *runtime.Causal, *ingressClock, string) {
	t.Helper()
	clock := &ingressClock{at: ingressTime}
	path := filepath.Join(t.TempDir(), "state.db")
	db, ingress := openIngress(t, path, clock)
	if _, _, err := ingress.Accept(context.Background(), "scope_a", causalEvent("root")); err != nil {
		t.Fatal(err)
	}
	c, err := runtime.NewCausal(db, clock.Now, limits)
	if err != nil {
		t.Fatal(err)
	}
	return db, c, clock, path
}
func TestCausalDurablePinnedLimitsAndAudit(t *testing.T) {
	ctx := context.Background()
	db, c, clock, path := causalFixture(t, runtime.CausalLimits{MaxDepth: 2, PerMinute: 2})
	if _, _, err := c.Derive(ctx, "scope_a", "root", causalEvent("child")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Derive(ctx, "scope_a", "child", causalEvent("grandchild")); err != nil {
		t.Fatal(err)
	}
	if _, added, err := c.Derive(ctx, "scope_a", "root", causalEvent("child")); err != nil || added {
		t.Fatalf("replay: added=%t err=%v", added, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c, err = runtime.NewCausal(db, clock.Now, runtime.CausalLimits{MaxDepth: 100, PerMinute: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []struct {
		parent domain.ID
		id     string
	}{{"root", "rate"}, {"grandchild", "depth"}, {"missing", "missing"}} {
		if _, _, err = c.Derive(ctx, "scope_a", attempt.parent, causalEvent(attempt.id)); !errors.Is(err, runtime.ErrCausalRejected) {
			t.Fatalf("%s: %v", attempt.id, err)
		}
	}
	clock.Set(ingressTime.Add(time.Minute))
	if _, _, err = c.Derive(ctx, "scope_a", "root", causalEvent("rate")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.Derive(ctx, "scope_a", "grandchild", causalEvent("depth")); !errors.Is(err, runtime.ErrCausalRejected) {
		t.Fatal(err)
	}
	audits, err := c.Rejections(ctx)
	if err != nil || len(audits) != 3 {
		t.Fatalf("audits=%+v err=%v", audits, err)
	}
	for _, audit := range audits {
		if audit.Reason == "depth_limit" && (audit.Limits.MaxDepth != 2 || audit.RootID != "root" || audit.Depth != 3) {
			t.Fatalf("lost pinned evidence: %+v", audit)
		}
	}
	if err = db.Transact(ctx, storage.Guard{Now: clock.Now()}, func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		if err == nil && len(all) != 4 {
			t.Fatalf("rejected event leaked: %d", len(all))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
func TestCausalConcurrentBudgetAndAtomicRefusal(t *testing.T) {
	ctx := context.Background()
	db, c, clock, _ := causalFixture(t, runtime.CausalLimits{PerMinute: 1})
	gate := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"left", "right"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-gate
			_, _, err := c.Derive(ctx, "scope_a", "root", causalEvent(id))
			results <- err
		}(id)
	}
	close(gate)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, runtime.ErrCausalRejected) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("successes=%d", success)
	}
	// Preflight commits an audit without creating the associated child Work.
	var refused error
	err := db.Transact(ctx, storage.Guard{Now: clock.Now()}, func(tx storage.Tx) error {
		e := causalEvent("child-created")
		e.WorkID = "child-work"
		refused = runtime.CheckDerivedInTx(tx, clock.Now(), runtime.CausalLimits{}, "root", e)
		if errors.Is(refused, runtime.ErrCausalRejected) {
			return nil
		}
		if refused != nil {
			return refused
		}
		return tx.PutWork(domain.Work{ID: "child-work", ScopeID: "scope_a", Kind: "fixture.work", State: domain.WorkReady}, 0)
	})
	if err != nil || !errors.Is(refused, runtime.ErrCausalRejected) {
		t.Fatalf("preflight: %v %v", err, refused)
	}
	if err = db.Transact(ctx, storage.Guard{Now: clock.Now()}, func(tx storage.Tx) error {
		_, er := tx.Work("child-work")
		if !errors.Is(er, storage.ErrNotFound) {
			t.Fatalf("refused child exists: %v", er)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	audits, err := c.Rejections(ctx)
	if err != nil || len(audits) != 2 {
		t.Fatalf("audits=%+v err=%v", audits, err)
	}
}
func TestCausalExecutorCannotForgeAncestryOrAuthority(t *testing.T) {
	ctx := context.Background()
	db, c, clock, _ := causalFixture(t, runtime.CausalLimits{PerMinute: 1})
	var lease domain.LeaseRef
	err := db.Transact(ctx, storage.Guard{Now: clock.Now()}, func(tx storage.Tx) error {
		if err := tx.PutWork(domain.Work{ID: "work", ScopeID: "scope_a", Kind: "fixture.work", State: domain.WorkReady}, 0); err != nil {
			return err
		}
		if err := tx.PutExecution(domain.Execution{ID: "attempt", WorkID: "work", ExecutorBinding: "fixture", Attempt: 1, State: domain.ExecutionPending, TriggerID: "root"}, 0); err != nil {
			return err
		}
		var er error
		lease, er = tx.Claim("attempt", "worker", time.Minute)
		return er
	})
	if err != nil {
		t.Fatal(err)
	}
	forged := causalEvent("forged")
	forged.CausationID = "invented"
	if _, _, err = c.Emit(ctx, lease, forged); !errors.Is(err, runtime.ErrCausalRejected) {
		t.Fatalf("forged ancestry: %v", err)
	}
	privileged := causalEvent("privileged")
	privileged.Type = "core.work.created"
	if _, _, err = c.Emit(ctx, lease, privileged); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("privileged family: %v", err)
	}
	a, added, err := c.Emit(ctx, lease, causalEvent("emitted"))
	if err != nil || !added || a.Event.CausationID != "root" || a.Event.WorkID != "work" || a.Event.ExecutionID != "attempt" {
		t.Fatalf("trusted parent/identity: %+v %t %v", a, added, err)
	}
	if _, _, err = c.Emit(ctx, lease, causalEvent("over-limit")); !errors.Is(err, runtime.ErrCausalRejected) {
		t.Fatalf("executor budget: %v", err)
	}
	clock.Set(clock.Now().Add(time.Minute))
	if _, _, err = c.Emit(ctx, lease, causalEvent("stale")); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("expired authority: %v", err)
	}
}
