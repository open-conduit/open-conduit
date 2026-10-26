package integration_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

var toolbeltKey = domain.CorrelationKey{Source: "fixture", ExternalID: "item"}

type toolbeltHarness struct {
	f         conformance.RuntimeFixture
	belt      *runtime.Toolbelt
	scheduler *runtime.Scheduler
	executor  *schedulerExecutor
	attempt   runtime.Attempt
	token     string
	echoes    *int
	deny      *bool
	mu        *sync.Mutex
	// plan translates the fixture.wait operation (see translated wait cases).
	plan    func(extension.Authority, domain.Payload) (extension.WaitPlan, error)
	aliasOK bool
}

func allToolbeltOperations() []string {
	return []string{extension.Emit, extension.CreateWork, extension.Wait, extension.DeclareOutcome, extension.ReadContext, "fixture.echo", "fixture.wait"}
}

// newToolbeltHarness runs a real scheduled attempt whose token is issued by the
// production toolbelt and delivered through RunRequest.Context.
func newToolbeltHarness(t *testing.T, mode string) *toolbeltHarness {
	t.Helper()
	o := routingOptions(routingHandlerFunc(routingDecision), "queue")
	o.Capabilities = domain.Capabilities{Toolbelt: extension.ToolbeltCLI}
	f, executor := schedulerFixture(t, mode, o)
	base := f.Driver.(*schedulerDriver)
	h := &toolbeltHarness{f: f, executor: executor, echoes: new(int), deny: new(bool), mu: &sync.Mutex{}}
	belt, err := runtime.NewToolbelt(f.Store, f.Clock.Now, base.router, runtime.ToolbeltOptions{
		Grant: func(context.Context, runtime.AttemptState) (runtime.ToolbeltGrant, error) {
			return runtime.ToolbeltGrant{Operations: allToolbeltOperations(), EventTypes: []string{"fixture.changed.v1"}}, nil
		},
		Authorize: func(_ context.Context, c runtime.AuthorityCheck) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if *h.deny || c.Binding != "fixture" || c.Generation != "fixture-generation" {
				return errors.New("denied")
			}
			return nil
		},
		AuthorizeAlias: func(context.Context, runtime.AliasAuthorization) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if !h.aliasOK {
				return errors.New("denied")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "fixture", EventTypes: []string{"fixture.changed.v1"}, Operations: []extension.Operation{{Name: "fixture.echo", Handler: func(_ context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
		h.mu.Lock()
		*h.echoes++
		h.mu.Unlock()
		return in, nil
	}}, {Name: "fixture.wait", Wait: func(_ context.Context, a extension.Authority, in domain.Payload) (extension.WaitPlan, error) {
		if h.plan == nil {
			return extension.WaitPlan{}, extension.ErrInvalid
		}
		return h.plan(a, in)
	}}}, DependencyFilter: func(filter domain.Payload, e domain.Event) bool {
		return string(filter.Data) == string(e.Data.Data)
	}}); err != nil {
		t.Fatal(err)
	}
	options := base.options
	options.Toolbelt, options.ToolbeltEndpoint = belt, "unix:///fixture/management.sock"
	options.AliasFilter = belt.DependencyFilter
	options.AuthorizeAlias = func(context.Context, runtime.AliasAuthorization) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.aliasOK {
			return errors.New("denied")
		}
		return nil
	}
	h.scheduler = newTestScheduler(t, f, options)
	h.belt = belt
	executor.onRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if r.Context.ToolbeltEndpoint != "unix:///fixture/management.sock" || r.Context.ToolbeltToken == "" {
			return nil, errors.New("missing toolbelt credentials")
		}
		h.mu.Lock()
		h.token = r.Context.ToolbeltToken
		h.mu.Unlock()
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		handle := &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}
		executor.mu.Lock()
		if executor.handles == nil {
			executor.handles = map[domain.ID]extension.RunHandle{}
		}
		executor.handles[r.Execution.ID] = handle
		executor.mu.Unlock()
		return handle, nil
	}
	key := toolbeltKey
	requireAccept(t, f, "trigger", &key)
	requireRoute(t, f.Driver, "trigger")
	h.attempt, err = h.scheduler.Start(context.Background(), "work/item")
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func (h *toolbeltHarness) authority() extension.Authority {
	return extension.Authority{ScopeID: "routing-scope", WorkID: "work/item", ExecutionID: h.attempt.Lease.ExecutionID, Lease: h.attempt.Lease, ExpiresAt: h.f.Clock.Now().Add(time.Minute), Operations: allToolbeltOperations(), EventTypes: []string{"fixture.changed.v1"}, TriggerID: "trigger"}
}

func TestProductionToolbeltSharedCases(t *testing.T) {
	for _, mode := range []string{"persistent", "reopen"} {
		t.Run(mode, func(t *testing.T) {
			conformance.RunToolbelt(t, func() conformance.ToolbeltFixture {
				h := newToolbeltHarness(t, mode)
				return conformance.ToolbeltFixture{Belt: h.belt, Issuer: h.belt, Registrar: h.belt, Token: h.token, Authority: h.authority(), Advance: h.f.Clock.Advance, LoseLease: func() error {
					h.f.Clock.Advance(conformance.RuntimeLease + time.Second)
					_, err := h.scheduler.Recover(context.Background(), h.attempt.Lease.ExecutionID)
					return err
				}}
			})
		})
	}
}

func TestProductionToolbeltTokenLifecycle(t *testing.T) {
	h := newToolbeltHarness(t, "reopen")
	ctx := context.Background()
	read := extension.ToolCall{Operation: extension.ReadContext}
	// Plaintext is never persisted in coordination storage.
	routingRead(t, h.f, func(tx storage.Tx) error {
		for _, ns := range []string{"core.toolbelt.tokens", "core.toolbelt.token_ids", "core.toolbelt.tokens.execution", "core.scheduler.attempts"} {
			rows, err := tx.List(ns, "")
			if err != nil {
				return err
			}
			for _, row := range rows {
				if strings.Contains(row.Key, h.token) || strings.Contains(string(row.Value), h.token) {
					t.Fatalf("token plaintext persisted in %s", ns)
				}
			}
		}
		return nil
	})
	if known, err := h.belt.Known(ctx, h.token); err != nil || !known {
		t.Fatalf("issued token unknown: %v", err)
	}
	// Current ceiling narrows a valid token immediately.
	h.mu.Lock()
	*h.deny = true
	h.mu.Unlock()
	if _, err := h.belt.Call(ctx, h.token, read); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("revoked ceiling accepted: %v", err)
	}
	h.mu.Lock()
	*h.deny = false
	h.mu.Unlock()
	// Recovery issues a fresh token at the new epoch and fences the old one.
	old := h.token
	h.f.Clock.Advance(conformance.RuntimeLease + time.Second)
	recovered, err := h.scheduler.Recover(ctx, h.attempt.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.belt.Call(ctx, old, read); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("superseded token accepted: %v", err)
	}
	id, fresh, err := h.belt.IssueWithID(ctx, extension.Authority{ScopeID: "routing-scope", WorkID: "work/item", ExecutionID: recovered.Lease.ExecutionID, Lease: recovered.Lease, ExpiresAt: h.f.Clock.Now().Add(time.Minute), Operations: []string{extension.ReadContext}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.belt.Call(ctx, fresh, read); err != nil {
		t.Fatal(err)
	}
	if _, err = h.belt.RevokeID(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err = h.belt.Call(ctx, fresh, read); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("revoked ID accepted: %v", err)
	}
	if _, err = h.belt.RevokeID(ctx, "xtok_unknown"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unknown token ID: %v", err)
	}
	if _, err = h.belt.Issue(ctx, extension.Authority{ScopeID: "routing-scope", WorkID: "work/item", ExecutionID: recovered.Lease.ExecutionID, Lease: h.attempt.Lease, ExpiresAt: h.f.Clock.Now().Add(time.Minute)}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("stale lease issued token: %v", err)
	}
}

func TestProductionToolbeltRequestIdempotency(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	ctx := context.Background()
	e := domain.Event{Type: "fixture.changed.v1", DeliveryID: "report", Data: domain.Payload{Schema: "fixture.v1", Data: []byte(`{"n":1}`)}}
	first, err := h.belt.CallRequest(ctx, h.token, "r1", extension.ToolCall{Operation: extension.Emit, Event: &e})
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.belt.CallRequest(ctx, h.token, "r1", extension.ToolCall{Operation: extension.Emit, Event: &e})
	if err != nil || second.Event.Event.ID != first.Event.Event.ID || second.Event.Sequence != first.Event.Sequence {
		t.Fatalf("duplicate emit not replayed: %v", err)
	}
	// Delivery identity also deduplicates without a request ID.
	third, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.Emit, Event: &e})
	if err != nil || third.Event.Event.ID != first.Event.Event.ID {
		t.Fatalf("delivery duplicate created another event: %v", err)
	}
	changed := e
	changed.DeliveryID = "other"
	if _, err = h.belt.CallRequest(ctx, h.token, "r1", extension.ToolCall{Operation: extension.Emit, Event: &changed}); !errors.Is(err, runtime.ErrRequestConflict) {
		t.Fatalf("request reuse accepted: %v", err)
	}
	child := domain.Work{Kind: "fixture.child"}
	a, err := h.belt.CallRequest(ctx, h.token, "child", extension.ToolCall{Operation: extension.CreateWork, Child: &child})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.belt.CallRequest(ctx, h.token, "child", extension.ToolCall{Operation: extension.CreateWork, Child: &child})
	if err != nil || a.Work.ID != b.Work.ID {
		t.Fatalf("duplicate child created: %v", err)
	}
	in := domain.Payload{Schema: "fixture.echo.v1", Data: []byte(`{"x":1}`)}
	for i := 0; i < 2; i++ {
		if _, err = h.belt.CallRequest(ctx, h.token, "echo", extension.ToolCall{Operation: "fixture.echo", Input: in}); err != nil {
			t.Fatal(err)
		}
	}
	if *h.echoes != 1 {
		t.Fatalf("extension handler ran %d times for one request", *h.echoes)
	}
	// Forged sources and undeclared core families are refused.
	forged := e
	forged.DeliveryID, forged.Source = "forged", "core.manual"
	if _, err = h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.Emit, Event: &forged}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("forged source accepted: %v", err)
	}
	foreign := domain.CorrelationKey{Source: "fixture", ExternalID: "neighbor"}
	unowned := e
	unowned.DeliveryID, unowned.CorrelationKey = "unowned", &foreign
	if _, err = h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.Emit, Event: &unowned}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("unowned correlation accepted: %v", err)
	}
}

func TestProductionToolbeltDeclaredWaitRegistersAfterSegmentFinishes(t *testing.T) {
	h := newToolbeltHarness(t, "reopen")
	ctx := context.Background()
	timeout := h.f.Clock.Now().Add(time.Hour)
	out, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.Wait, Wait: &extension.WaitRequest{Match: domain.EventMatch{Keys: []domain.CorrelationKey{toolbeltKey}, Types: []string{"fixture.changed"}}, Timeout: &timeout}})
	if err != nil || out.Wait == nil {
		t.Fatalf("wait declaration: %v", err)
	}
	// The request alone releases authority but registers nothing yet.
	routingRead(t, h.f, func(tx storage.Tx) error {
		if _, err := tx.Wait(out.Wait.ID); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("wait registered before executor handshake: %v", err)
		}
		x, err := tx.Execution(h.attempt.Lease.ExecutionID)
		if err != nil || x.State != domain.ExecutionRunning {
			t.Fatalf("execution changed before handshake: %v %v", x.State, err)
		}
		return nil
	})
	if _, err = h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.ReadContext}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("declared wait retained authority: %v", err)
	}
	usage := uint64(7)
	if err = h.scheduler.Complete(ctx, h.attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "segment done"}, Usage: extension.Usage{InputTokens: &usage}}); err != nil {
		t.Fatal(err)
	}
	routingRead(t, h.f, func(tx storage.Tx) error {
		wait, err := tx.Wait(out.Wait.ID)
		if err != nil || wait.State != domain.WaitActive || wait.TimeoutTimerID != out.Wait.TimeoutTimerID {
			t.Fatalf("declared wait not registered: %+v %v", wait, err)
		}
		if _, err = tx.Timer(wait.TimeoutTimerID); err != nil {
			t.Fatalf("timeout timer missing: %v", err)
		}
		x, _ := tx.Execution(h.attempt.Lease.ExecutionID)
		w, _ := tx.Work("work/item")
		if x.State != domain.ExecutionWaiting || x.Outcome == nil || x.Outcome.Kind != domain.OutcomeWaiting || w.State != domain.WorkWaiting {
			t.Fatalf("waiting outcome not applied: %v %v", x.State, w.State)
		}
		return nil
	})
}

func TestProductionToolbeltDeclaredOutcomeAppliesOnFinish(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	ctx := context.Background()
	if _, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.DeclareOutcome, Outcome: &domain.Outcome{Kind: domain.OutcomeFailed, Summary: "declared failure"}}); err != nil {
		t.Fatal(err)
	}
	routingRead(t, h.f, func(tx storage.Tx) error {
		x, _ := tx.Execution(h.attempt.Lease.ExecutionID)
		if x.State != domain.ExecutionRunning {
			t.Fatalf("declaration established exclusion without handshake: %v", x.State)
		}
		return nil
	})
	if _, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.DeclareOutcome, Outcome: &domain.Outcome{Kind: domain.OutcomeCompleted}}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("second declaration accepted: %v", err)
	}
	if err := h.scheduler.Complete(ctx, h.attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); err != nil {
		t.Fatal(err)
	}
	routingRead(t, h.f, func(tx storage.Tx) error {
		x, _ := tx.Execution(h.attempt.Lease.ExecutionID)
		if x.State != domain.ExecutionFailed || x.Outcome == nil || x.Outcome.Summary != "declared failure" {
			t.Fatalf("declared outcome ignored: %v %+v", x.State, x.Outcome)
		}
		return nil
	})
}
