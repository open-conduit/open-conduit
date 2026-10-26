package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

func linkedClaim(t *testing.T, db storage.Store, now time.Time, work, trigger domain.ID) domain.LeaseRef {
	t.Helper()
	var ref domain.LeaseRef
	if err := db.Transact(context.Background(), storage.Guard{Now: now}, func(tx storage.Tx) error {
		id := domain.ID("attempt/" + string(work))
		if err := tx.PutExecution(domain.Execution{ID: id, WorkID: work, ExecutorBinding: "fixture", Attempt: 1, State: domain.ExecutionPending, TriggerID: trigger}, 0); err != nil {
			return err
		}
		var err error
		ref, err = tx.Claim(id, "fixture", time.Minute)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ref
}
func TestLinkedChildInheritsRootAndAtomicBudgets(t *testing.T) {
	ctx := context.Background()
	limits := runtime.CausalLimits{MaxDepth: 2, PerMinute: 3}
	db, causal, clock, _ := causalFixture(t, limits)
	router, err := runtime.NewRouter(db, clock.Now, runtime.RouterOptions{Generation: "fixture", CausalLimits: limits})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Transact(ctx, storage.Guard{Now: clock.Now()}, func(tx storage.Tx) error {
		return tx.PutWork(domain.Work{ID: "parent", Kind: "fixture.work", ScopeID: "scope_a", State: domain.WorkReady}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	parentLease := linkedClaim(t, db, clock.Now(), "parent", "root")
	request := runtime.ChildRequest{ID: "child", Kind: "fixture.work", EventID: "child-event"}
	child, err := router.CreateChild(ctx, parentLease, request)
	if err != nil || child.ParentID != "parent" || child.ScopeID != "scope_a" {
		t.Fatalf("child %+v %v", child, err)
	}
	childLease := linkedClaim(t, db, clock.Now(), child.ID, request.EventID)
	grandRequest := runtime.ChildRequest{ID: "grandchild", Kind: "fixture.work", EventID: "grandchild-event"}
	grandchild, err := router.CreateChild(ctx, childLease, grandRequest)
	if err != nil || grandchild.ParentID != child.ID || grandchild.ScopeID != child.ScopeID {
		t.Fatalf("grandchild %+v %v", grandchild, err)
	}
	grandLease := linkedClaim(t, db, clock.Now(), grandchild.ID, grandRequest.EventID)
	if _, _, err = causal.Emit(ctx, grandLease, causalEvent("too-deep-from-grandchild")); !errors.Is(err, runtime.ErrCausalRejected) {
		t.Fatalf("child severed root depth: %v", err)
	}
	if _, _, err = causal.Emit(ctx, childLease, causalEvent("child-emission")); err != nil {
		t.Fatal(err)
	}
	// At the root's cap, replay is free and another child fails atomically.
	if _, err = router.CreateChild(ctx, parentLease, request); err != nil {
		t.Fatalf("child replay charged budget: %v", err)
	}
	refused := runtime.ChildRequest{ID: "refused-child", Kind: "fixture.work", EventID: "refused-child-event"}
	if _, err = router.CreateChild(ctx, parentLease, refused); !errors.Is(err, runtime.ErrCausalRejected) {
		t.Fatalf("child bypassed sibling rate: %v", err)
	}
	if err = db.Transact(ctx, storage.Guard{Now: clock.Now()}, func(tx storage.Tx) error {
		if _, er := tx.Work(refused.ID); !errors.Is(er, storage.ErrNotFound) {
			t.Fatalf("rejected Work leaked: %v", er)
		}
		if _, er := tx.Event(refused.EventID); !errors.Is(er, storage.ErrNotFound) {
			t.Fatalf("rejected creation leaked: %v", er)
		}
		a, er := tx.Event(request.EventID)
		if er != nil {
			return er
		}
		if a.Event.Type != "core.work.created" || a.Event.CausationID != "root" || a.Event.WorkID != child.ID {
			t.Fatalf("creation envelope %+v", a)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	audits, err := causal.Rejections(ctx)
	if err != nil || len(audits) != 2 {
		t.Fatalf("audit %+v %v", audits, err)
	}
	for _, a := range audits {
		if a.RootID != "root" {
			t.Fatalf("lost root in child diagnostic %+v", a)
		}
	}
}
func TestLinkedSuccessorPreservesTerminalGeneration(t *testing.T) {
	ctx := context.Background()
	limits := runtime.CausalLimits{PerMinute: 1}
	db, _, clock, _ := causalFixture(t, limits)
	router, err := runtime.NewRouter(db, clock.Now, runtime.RouterOptions{Generation: "fixture", CausalLimits: limits})
	if err != nil {
		t.Fatal(err)
	}
	oldKey := domain.CorrelationKey{Source: "fixture.resources", ExternalID: "resource/42"}
	newKey := domain.CorrelationKey{Source: oldKey.Source, ExternalID: "resource/42/generation/reopen-7"}
	if err = db.Transact(ctx, storage.Guard{Now: clock.Now()}, func(tx storage.Tx) error {
		return tx.PutWork(domain.Work{ID: "old", Kind: "fixture.work", ScopeID: "scope_a", State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{oldKey}}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if err = router.Cancel(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	late := causalEvent("late")
	late.WorkID = "old"
	late.CorrelationKey = &oldKey
	ingress, err := runtime.NewIngress(db, clock.Now, runtime.IngressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = ingress.Accept(ctx, "scope_a", late); err != nil {
		t.Fatal(err)
	}
	request := runtime.ChildRequest{ID: "successor", Kind: "fixture.work", EventID: "successor-event", Keys: []domain.CorrelationKey{newKey}}
	next, err := router.CreateSuccessor(ctx, "old", late.ID, request)
	if err != nil || next.ParentID != "" || next.ScopeID != "scope_a" {
		t.Fatalf("successor %+v %v", next, err)
	}
	if _, err = router.CreateSuccessor(ctx, "old", late.ID, request); err != nil {
		t.Fatalf("successor replay charged budget %v", err)
	}
	bad := runtime.ChildRequest{ID: "steal-old", Kind: "fixture.work", Keys: []domain.CorrelationKey{oldKey}}
	if _, err = router.CreateSuccessor(ctx, "old", late.ID, bad); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("reused old identity %v", err)
	}
	if err = db.Transact(ctx, storage.Guard{Now: clock.Now()}, func(tx storage.Tx) error {
		old, er := tx.WorkByCorrelation(oldKey)
		if er != nil {
			return er
		}
		current, er := tx.WorkByCorrelation(newKey)
		if er != nil {
			return er
		}
		if old.ID != "old" || old.State != domain.WorkCancelled || current.ID != next.ID {
			t.Fatalf("terminal ownership changed old=%+v current=%+v", old, current)
		}
		a, er := tx.Event(request.EventID)
		if er != nil {
			return er
		}
		var payload struct{ WorkID, ParentID, PredecessorID domain.ID }
		if er = json.Unmarshal(a.Event.Data.Data, &payload); er != nil {
			return er
		}
		if payload.WorkID != next.ID || payload.ParentID != "" || payload.PredecessorID != old.ID || a.Event.CausationID != late.ID {
			t.Fatalf("successor link %+v event=%+v", payload, a)
		}
		all, er := tx.Events(0, 0)
		if er == nil && len(all) != 3 {
			t.Fatalf("replay duplicated creation: %d", len(all))
		}
		return er
	}); err != nil {
		t.Fatal(err)
	}
}
