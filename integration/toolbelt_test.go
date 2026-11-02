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
	// connection is the Authority.Connection the last fixture.echo saw.
	connection *string
	mu         *sync.Mutex
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
	return newToolbeltHarnessRouting(t, mode, routingHandlerFunc(routingDecision))
}

// newToolbeltHarnessRouting is newToolbeltHarness with its own route handler.
func newToolbeltHarnessRouting(t *testing.T, mode string, handler extension.RouteHandler) *toolbeltHarness {
	t.Helper()
	o := routingOptions(handler, "queue")
	o.Capabilities = domain.Capabilities{Toolbelt: extension.ToolbeltCLI}
	f, executor := schedulerFixture(t, mode, o)
	base := f.Driver.(*schedulerDriver)
	h := &toolbeltHarness{f: f, executor: executor, echoes: new(int), deny: new(bool), connection: new(string), mu: &sync.Mutex{}}
	belt, err := runtime.NewToolbelt(f.Store, f.Clock.Now, base.router, runtime.ToolbeltOptions{
		Grant: func(context.Context, runtime.AttemptState) (runtime.ToolbeltGrant, error) {
			return runtime.ToolbeltGrant{Operations: allToolbeltOperations(), EventTypes: []string{"fixture.changed.v1"}, Connection: "fixture-connection"}, nil
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
		*h.connection = a.Connection
		h.mu.Unlock()
		return in, nil
	}, Guidance: "Echo with fixture.echo."}, {Name: "fixture.wait", Wait: func(_ context.Context, a extension.Authority, in domain.Payload) (extension.WaitPlan, error) {
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
	options.Toolbelt, options.ToolbeltEndpoints = belt, fixtureEndpoint("unix:///fixture/toolbelt.sock")
	options.AliasFilter = belt.DependencyFilter
	options.AuthorizeAlias = func(context.Context, runtime.AliasAuthorization) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.aliasOK {
			return errors.New("denied")
		}
		return nil
	}
	// There is no shared endpoint to fall back to: a toolbelt needs
	// per-execution endpoints.
	shared := options
	shared.ToolbeltEndpoints = nil
	if _, err := runtime.NewScheduler(f.Store, f.Clock.Now, f.Driver.(*schedulerDriver).router, shared); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("scheduler accepted a toolbelt without per-execution endpoints: %v", err)
	}
	h.scheduler = newTestScheduler(t, f, options)
	h.belt = belt
	executor.onRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if r.Context.ToolbeltEndpoint != "unix:///fixture/toolbelt.sock" || r.Context.ToolbeltToken == "" {
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
	return extension.Authority{ScopeID: "routing-scope", WorkID: "work/item", ExecutionID: h.attempt.Lease.ExecutionID, Lease: h.attempt.Lease, ExpiresAt: h.f.Clock.Now().Add(time.Minute), Operations: allToolbeltOperations(), EventTypes: []string{"fixture.changed.v1"}, TriggerID: "trigger", Connection: "fixture-connection"}
}

// TestProductionToolbeltAuthorityCarriesConnection: an operation handler's
// authority names the connection the attempt's grant pinned (the binding's
// connection), and a token issued without one carries none.
func TestProductionToolbeltAuthorityCarriesConnection(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	ctx := context.Background()
	echo := func(token string) string {
		t.Helper()
		if _, err := h.belt.Call(ctx, token, extension.ToolCall{Operation: "fixture.echo", Input: domain.Payload{Schema: "fixture.echo.v1", Data: []byte(`{}`)}}); err != nil {
			t.Fatal(err)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		return *h.connection
	}
	if got := echo(h.token); got != "fixture-connection" {
		t.Fatalf("the scheduled attempt's authority names %q", got)
	}
	_, bare, err := h.belt.IssueWithID(ctx, extension.Authority{ScopeID: "routing-scope", WorkID: "work/item", ExecutionID: h.attempt.Lease.ExecutionID, Lease: h.attempt.Lease, ExpiresAt: h.f.Clock.Now().Add(time.Minute), Operations: []string{"fixture.echo"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := echo(bare); got != "" {
		t.Fatalf("a token without a connection carries %q", got)
	}
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

// TestProductionToolbeltGuidance serves an operation's guidance only to a
// token whose grant includes it, while current config still enables it.
func TestProductionToolbeltGuidance(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	ctx := context.Background()
	got, err := h.belt.Guidance(ctx, h.token, "")
	if err != nil || len(got) != 1 || got[0] != "Echo with fixture.echo." {
		t.Fatalf("guidance = %q, %v", got, err)
	}
	if _, err = h.belt.Guidance(ctx, h.token, "execution/other"); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("another execution's assertion accepted: %v", err)
	}
	if _, err = h.belt.Guidance(ctx, "unknown", ""); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("unknown token accepted: %v", err)
	}
	h.mu.Lock()
	*h.deny = true
	h.mu.Unlock()
	if got, err = h.belt.Guidance(ctx, h.token, ""); err != nil || len(got) != 0 {
		t.Fatalf("guidance under a narrowed ceiling = %q, %v", got, err)
	}
	h.mu.Lock()
	*h.deny = false
	h.mu.Unlock()
	_, narrow, err := h.belt.IssueWithID(ctx, extension.Authority{ScopeID: "routing-scope", WorkID: "work/item", ExecutionID: h.attempt.Lease.ExecutionID, Lease: h.attempt.Lease, ExpiresAt: h.f.Clock.Now().Add(time.Minute), Operations: []string{extension.ReadContext}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err = h.belt.Guidance(ctx, narrow, ""); err != nil || len(got) != 0 {
		t.Fatalf("guidance for an ungranted operation = %q, %v", got, err)
	}
	long := extension.Operation{Name: "fixture.long", Handler: func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
		return domain.Payload{}, nil
	}, Guidance: strings.Repeat("x", extension.MaxGuidance+1)}
	if err = h.belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "long", Operations: []extension.Operation{long}}); !errors.Is(err, extension.ErrInvalid) {
		t.Fatalf("oversized guidance registered: %v", err)
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

// TestProductionToolbeltGates: a registration's gate decides on every call
// of another registration's operation, never its own, and builtins pass no
// gate. Gates are asked in name order; one that panics refuses (fail
// closed), and a refusal reaches the caller as the gate's own error.
func TestProductionToolbeltGates(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	ctx := context.Background()
	var asked []string
	if err := h.belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "zeta",
		Operations: []extension.Operation{{Name: "zeta.op", Handler: func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
			return domain.Payload{}, nil
		}}},
		Gate: func(_ context.Context, a extension.Authority, c extension.GateCall) (*extension.WaitPlan, error) {
			asked = append(asked, c.Owner+"/"+c.Operation)
			if a.WorkID != "work/item" || string(c.Input.Data) != `{"n":1}` {
				return nil, errors.New("gate saw the wrong call")
			}
			return nil, &extension.OperationError{Code: "not_now", Message: "zeta says no"}
		}}); err != nil {
		t.Fatal(err)
	}
	echo := extension.ToolCall{Operation: "fixture.echo", Input: domain.Payload{Schema: "fixture.echo.v1", Data: []byte(`{"n":1}`)}}
	_, err := h.belt.Call(ctx, h.token, echo)
	var oe *extension.OperationError
	var gr *extension.GateRefusal
	if !errors.As(err, &oe) || oe.Code != "not_now" || !errors.As(err, &gr) || gr.Gate != "zeta" || *h.echoes != 0 {
		t.Fatalf("zeta's gate did not refuse fixture.echo: %v (echoes %d)", err, *h.echoes)
	}
	if strings.Join(asked, ",") != "fixture/fixture.echo" {
		t.Fatalf("gate asked: %v", asked)
	}
	// zeta.op isn't in the grant, so its own call is unauthorized before any
	// gate; builtins pass every gate.
	if _, err = h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.ReadContext}); err != nil {
		t.Fatalf("a builtin passed through a gate: %v", err)
	}
	if len(asked) != 1 {
		t.Fatalf("a builtin was gated: %v", asked)
	}
	if err = h.belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "alpha",
		Operations: []extension.Operation{{Name: "alpha.op", Handler: func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
			return domain.Payload{}, nil
		}}},
		Gate: func(context.Context, extension.Authority, extension.GateCall) (*extension.WaitPlan, error) {
			panic("broken gate")
		}}); err != nil {
		t.Fatal(err)
	}
	_, err = h.belt.Call(ctx, h.token, echo)
	if !errors.As(err, &oe) || oe.Code != "gate_failed" || !errors.As(err, &gr) || gr.Gate != "alpha" || len(asked) != 1 || *h.echoes != 0 {
		t.Fatalf("a panicking gate did not refuse first: %v (asked %v, echoes %d)", err, asked, *h.echoes)
	}
}

// TestProductionToolbeltGateSkipsItsOwnOperations: a gate never decides on
// its own registration's operations.
func TestProductionToolbeltGateSkipsItsOwnOperations(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	ctx := context.Background()
	var asked []string
	belt, err := runtime.NewToolbelt(h.f.Store, h.f.Clock.Now, h.f.Driver.(*schedulerDriver).router, runtime.ToolbeltOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gate := func(_ context.Context, _ extension.Authority, c extension.GateCall) (*extension.WaitPlan, error) {
		asked = append(asked, c.Operation)
		return nil, errors.New("refused")
	}
	if err = belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "fixture", Gate: gate, Operations: []extension.Operation{{Name: "fixture.echo", Handler: func(_ context.Context, _ extension.Authority, in domain.Payload) (domain.Payload, error) {
		return in, nil
	}}}}); err != nil {
		t.Fatal(err)
	}
	token, err := belt.Issue(ctx, extension.Authority{ScopeID: "routing-scope", WorkID: "work/item", ExecutionID: h.attempt.Lease.ExecutionID, Lease: h.attempt.Lease, ExpiresAt: h.f.Clock.Now().Add(time.Minute), Operations: []string{"fixture.echo"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = belt.Call(ctx, token, extension.ToolCall{Operation: "fixture.echo", Input: domain.Payload{Schema: "fixture.echo.v1", Data: []byte(`{}`)}}); err != nil || len(asked) != 0 {
		t.Fatalf("a gate decided on its own operation: %v %v", err, asked)
	}
}
