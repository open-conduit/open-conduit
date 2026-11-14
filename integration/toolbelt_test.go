package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/observability"
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
	// served counts fixture.serve's runs.
	served *int
	// connection is the Authority.Connection the last fixture.echo saw.
	connection *string
	mu         *sync.Mutex
	// onEcho runs inside fixture.echo, after its effect, with the call's
	// request ID.
	onEcho func(requestID string)
	// plan translates the fixture.wait operation (see translated wait cases).
	plan    func(extension.Authority, domain.Payload) (extension.WaitPlan, error)
	aliasOK bool
	// filterDown makes the fixture's dependency filter fail to answer, as
	// a remote extension's does while it times out or restarts.
	filterDown bool
	// filterFailsOn makes the live filter fail on that Event ID, as an
	// extension whose handler panics on it does.
	filterFailsOn string
	// filterCalls counts the fixture's dependency filter calls.
	filterCalls int
	// records are the toolbelt's call records; refusal classifies a
	// refused call (ToolbeltOptions.Refusal) when set.
	records *[]observability.Record
	refusal func(runtime.AuthorityCheck) string
}

// harnessObserver keeps the toolbelt's call records.
type harnessObserver struct{ h *toolbeltHarness }

func (o harnessObserver) Observe(_ context.Context, r observability.Record) error {
	o.h.mu.Lock()
	defer o.h.mu.Unlock()
	*o.h.records = append(*o.h.records, r)
	return nil
}

func allToolbeltOperations() []string {
	return []string{extension.Emit, extension.CreateWork, extension.CancelWork, extension.Wait, extension.DeclareOutcome, extension.ReadContext, "fixture.echo", "fixture.wait"}
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
	h := &toolbeltHarness{f: f, executor: executor, echoes: new(int), served: new(int), deny: new(bool), connection: new(string), mu: &sync.Mutex{}, records: new([]observability.Record)}
	belt, err := runtime.NewToolbelt(f.Store, f.Clock.Now, base.Router, runtime.ToolbeltOptions{
		Grant: func(context.Context, runtime.AttemptState) (runtime.ToolbeltGrant, error) {
			return runtime.ToolbeltGrant{Operations: append(allToolbeltOperations(), "fixture.serve"), EventTypes: []string{"fixture.changed.v1"}, Connection: "fixture-connection", WorkKinds: fixtureWorkKinds}, nil
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
		Observer: harnessObserver{h},
		Refusal: func(_ context.Context, c runtime.AuthorityCheck) string {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.refusal == nil {
				return ""
			}
			return h.refusal(c)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "fixture", EventTypes: []string{"fixture.changed.v1"}, Operations: []extension.Operation{{Name: "fixture.echo", Handler: func(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
		h.mu.Lock()
		*h.echoes++
		*h.connection = a.Connection
		onEcho := h.onEcho
		h.mu.Unlock()
		if onEcho != nil {
			onEcho(extension.CallRequestID(ctx))
		}
		return in, nil
	}, Guidance: "Echo with fixture.echo.", Summary: "Echo the input.", Input: json.RawMessage(`{"type":"object"}`)}, {Name: "fixture.serve", Handler: func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
		h.mu.Lock()
		*h.served++
		h.mu.Unlock()
		return domain.Payload{Schema: "fixture.secret.v1", Data: []byte(`{"password":"fixture-secret"}`)}, nil
	}, Guidance: "Never shown: fixture.serve is its client's.", Serves: "fixture.capability", Summary: "Answer the fixture capability.", Input: json.RawMessage(`{"type":"object"}`)}, {Name: "fixture.wait", Wait: func(_ context.Context, a extension.Authority, in domain.Payload) (extension.WaitPlan, error) {
		if h.plan == nil {
			return extension.WaitPlan{}, extension.ErrInvalid
		}
		return h.plan(a, in)
	}}}, DependencyFilter: func(filter domain.Payload, e domain.Event) (bool, error) {
		h.mu.Lock()
		unavailable, failing := h.filterDown, h.filterFailsOn
		h.filterCalls++
		h.mu.Unlock()
		if unavailable {
			return false, errors.New("filter unavailable")
		}
		if failing != "" && string(e.ID) == failing {
			return false, fmt.Errorf("%w: handler panicked", extension.ErrFilterFailed)
		}
		return string(filter.Data) == string(e.Data.Data), nil
	}}); err != nil {
		t.Fatal(err)
	}
	options := base.Options
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
	if _, err := runtime.NewScheduler(f.Store, f.Clock.Now, f.Driver.(*schedulerDriver).Router, shared); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("scheduler accepted a toolbelt without per-execution endpoints: %v", err)
	}
	h.scheduler = newTestScheduler(t, f, options)
	h.belt = belt
	executor.OnRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		if r.Context.ToolbeltEndpoint != "unix:///fixture/toolbelt.sock" || r.Context.ToolbeltToken == "" {
			return nil, errors.New("missing toolbelt credentials")
		}
		h.mu.Lock()
		h.token = r.Context.ToolbeltToken
		h.mu.Unlock()
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		handle := &schedulerHandle{Result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}
		executor.Mu.Lock()
		if executor.Handles == nil {
			executor.Handles = map[domain.ID]extension.RunHandle{}
		}
		executor.Handles[r.Execution.ID] = handle
		executor.Mu.Unlock()
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
	return extension.Authority{ScopeID: "routing-scope", WorkID: "work/item", ExecutionID: h.attempt.Lease.ExecutionID, Lease: h.attempt.Lease, ExpiresAt: h.f.Clock.Now().Add(time.Minute), Operations: allToolbeltOperations(), EventTypes: []string{"fixture.changed.v1"}, TriggerID: "trigger", Connection: "fixture-connection", WorkKinds: fixtureWorkKinds}
}

// fixtureWorkKinds are the kinds of child Work the harness's grant lets
// its execution ask for.
var fixtureWorkKinds = []extension.WorkKind{{Kind: "fixture.child"}}

// TestProductionToolbeltReplayWindow drills the crash window between an
// extension handler's effect and the commit of its replay record: the call
// ends after the handler ran but before the record commits, as when the
// daemon dies there. The retry with the same request ID runs the handler
// again, which is why handlers must be idempotent on the request ID; a
// handler that keys its effect by CallRequestID makes it once.
func TestProductionToolbeltReplayWindow(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	in := domain.Payload{Schema: "fixture.echo.v1", Data: []byte(`{"x":1}`)}
	effects := map[string]int{}
	crash, cancel := context.WithCancel(context.Background())
	h.onEcho = func(id string) {
		if effects[id] == 0 { // the handler's own dedupe on the request ID
			effects[id]++
		}
		cancel() // the host dies before the replay record commits
	}
	if _, err := h.belt.CallRequest(crash, h.token, "window", extension.ToolCall{Operation: "fixture.echo", Input: in}); !errors.Is(err, context.Canceled) {
		t.Fatalf("call survived the crash: %v", err)
	}
	h.onEcho = func(id string) {
		if effects[id] == 0 {
			effects[id]++
		}
	}
	ctx := context.Background()
	for range 2 {
		if _, err := h.belt.CallRequest(ctx, h.token, "window", extension.ToolCall{Operation: "fixture.echo", Input: in}); err != nil {
			t.Fatal(err)
		}
	}
	// Negative control: the handler did run again after the crash (once,
	// then the committed record replayed), so a handler that didn't dedupe
	// would have made its effect twice.
	if *h.echoes != 2 {
		t.Fatalf("handler ran %d times, want 2 (once before the crash, once on the retry)", *h.echoes)
	}
	if effects["window"] != 1 {
		t.Fatalf("idempotent handler made its effect %d times", effects["window"])
	}
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

// TestProductionToolbeltServing names the operations serving a capability
// only to a token whose grant includes them, while current config still
// enables them; only a handler may serve one.
func TestProductionToolbeltServing(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	ctx := context.Background()
	got, err := h.belt.Serving(ctx, h.token, "", "fixture.capability")
	if err != nil || len(got) != 1 || got[0] != "fixture.serve" {
		t.Fatalf("serving = %q, %v", got, err)
	}
	if got, err = h.belt.Serving(ctx, h.token, "", extension.CapabilityGitCredential); err != nil || len(got) != 0 {
		t.Fatalf("serving another capability = %q, %v", got, err)
	}
	if _, err = h.belt.Serving(ctx, h.token, "execution/other", "fixture.capability"); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("another execution's assertion accepted: %v", err)
	}
	h.mu.Lock()
	*h.deny = true
	h.mu.Unlock()
	if got, err = h.belt.Serving(ctx, h.token, "", "fixture.capability"); err != nil || len(got) != 0 {
		t.Fatalf("serving under a narrowed ceiling = %q, %v", got, err)
	}
	h.mu.Lock()
	*h.deny = false
	h.mu.Unlock()
	wait := extension.Operation{Name: "serves.wait", Serves: "fixture.capability", Wait: func(context.Context, extension.Authority, domain.Payload) (extension.WaitPlan, error) {
		return extension.WaitPlan{}, nil
	}}
	if err = h.belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "serves", Operations: []extension.Operation{wait}}); !errors.Is(err, extension.ErrInvalid) {
		t.Fatalf("a wait serving a capability registered: %v", err)
	}
}

// TestProductionToolbeltServesCapabilityOnlyToItsClient: an operation
// serving a capability answers a call made for it (git's credential helper),
// and refuses the agent's own call of it with capability_only and what to do
// instead, before any gate and without running, so its result (a secret)
// never reaches the agent. Naming a capability an operation doesn't serve is
// refused too, and the answer is never kept for replay.
func TestProductionToolbeltServesCapabilityOnlyToItsClient(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	ctx := context.Background()
	var gated []string
	h.belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "watcher", Gate: func(_ context.Context, _ extension.Authority, c extension.GateCall) (*extension.WaitPlan, error) {
		gated = append(gated, c.Operation+"/"+c.Capability)
		return nil, nil
	}})
	in := domain.Payload{Schema: "fixture.v1", Data: []byte(`{}`)}
	var op *extension.OperationError
	for name, call := range map[string]extension.ToolCall{
		"the agent's own call":      {Operation: "fixture.serve", Input: in},
		"another capability":        {Operation: "fixture.serve", Input: in, Capability: extension.CapabilityGitCredential},
		"a capability echo doesn't": {Operation: "fixture.echo", Input: in, Capability: "fixture.capability"},
	} {
		out, err := h.belt.Call(ctx, h.token, call)
		if !errors.As(err, &op) || op.Code != extension.CodeCapabilityOnly || out.Output.Data != nil {
			t.Fatalf("%s: %+v, %v, want %s", name, out, err, extension.CodeCapabilityOnly)
		}
	}
	if len(gated) != 0 {
		t.Fatalf("a refused call reached a gate: %q", gated)
	}
	out, err := h.belt.CallRequest(ctx, h.token, "kept", extension.ToolCall{Operation: "fixture.serve", Input: in, Capability: "fixture.capability"})
	if err != nil || !strings.Contains(string(out.Output.Data), "fixture-secret") {
		t.Fatalf("the client's call = %+v, %v", out, err)
	}
	if strings.Join(gated, ",") != "fixture.serve/fixture.capability" {
		t.Fatalf("gates were told %q, want the capability", gated)
	}
	// The same request ID asks afresh rather than replaying the secret.
	if _, err = h.belt.CallRequest(ctx, h.token, "kept", extension.ToolCall{Operation: "fixture.serve", Input: in, Capability: "fixture.capability"}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	served := *h.served
	h.mu.Unlock()
	if served != 2 {
		t.Fatalf("fixture.serve ran %d times, want 2: its answer was kept for replay", served)
	}
}

// TestProductionToolbeltServingWhileRecovering: the toolbelt's reads (the
// operations serving a capability, the guidance) refuse a token recovery
// superseded as ErrAuthorityRecovering while the execution is still
// running, as a call with it is, so git's credential helper and the MCP
// server retry with the recovered token. Once the execution has finished,
// the same token gets the bare ErrUnauthorized.
func TestProductionToolbeltServingWhileRecovering(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	ctx := context.Background()
	h.f.Clock.Advance(conformance.RuntimeLease + time.Second)
	recovered, err := h.scheduler.Recover(ctx, h.attempt.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.belt.Serving(ctx, h.token, "", "fixture.capability"); !errors.Is(err, extension.ErrAuthorityRecovering) {
		t.Fatalf("serving with a superseded token: %v, want ErrAuthorityRecovering", err)
	}
	if _, err := h.belt.Guidance(ctx, h.token, ""); !errors.Is(err, extension.ErrAuthorityRecovering) {
		t.Fatalf("guidance with a superseded token: %v, want ErrAuthorityRecovering", err)
	}
	if err := h.scheduler.Complete(ctx, recovered.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.belt.Serving(ctx, h.token, "", "fixture.capability"); !errors.Is(err, extension.ErrUnauthorized) || errors.Is(err, extension.ErrAuthorityRecovering) {
		t.Fatalf("serving once the execution finished: %v, want the bare ErrUnauthorized", err)
	}
}

// TestProductionToolbeltGuidance serves an operation's guidance only to a
// token whose grant includes it, while current config still enables it.
func TestProductionToolbeltGuidance(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	ctx := context.Background()
	got, err := h.belt.Guidance(ctx, h.token, "")
	// The host's own guidance follows: the kinds of child Work the grant
	// lets the execution ask for.
	if err != nil || len(got) != 2 || got[0] != "Echo with fixture.echo." || !strings.Contains(got[1], "- `fixture.child`") || !strings.Contains(got[1], "`cancel_work`") || !strings.Contains(got[1], "`wait` is the last call of a turn: after it, every other call is refused. To hand a task over, call `create_work` first, then `wait`.") {
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
	// Summaries and inputs follow the grant the same way.
	if d, err := h.belt.Described(ctx, h.token, ""); err != nil || len(d) != 1 || d[0].Name != "fixture.echo" || d[0].Summary != "Echo the input." || string(d[0].Input) != `{"type":"object"}` || d[0].Handler != nil {
		t.Fatalf("described = %+v, %v", d, err)
	}
	if d, err := h.belt.Described(ctx, narrow, ""); err != nil || len(d) != 0 {
		t.Fatalf("described for an ungranted operation = %+v, %v", d, err)
	}
	if _, err = h.belt.Described(ctx, h.token, "execution/other"); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("another execution's assertion accepted: %v", err)
	}
	long := extension.Operation{Name: "long.op", Handler: func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
		return domain.Payload{}, nil
	}, Guidance: strings.Repeat("x", extension.MaxGuidance+1)}
	if err = h.belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "long", Operations: []extension.Operation{long}}); !errors.Is(err, extension.ErrInvalid) {
		t.Fatalf("oversized guidance registered: %v", err)
	}
	for name, op := range map[string]extension.Operation{
		"summary_lines": {Summary: "One.\nTwo."},
		"summary_long":  {Summary: strings.Repeat("x", extension.MaxSummary+1)},
		"input_array":   {Input: json.RawMessage(`[]`)},
		"input_text":    {Input: json.RawMessage(`not json`)},
	} {
		op.Name = name + ".op"
		op.Handler = long.Handler
		if err = h.belt.Register(extension.Registration{Version: extension.ContractVersion, Name: name, Operations: []extension.Operation{op}}); !errors.Is(err, extension.ErrInvalid) {
			t.Errorf("%s registered: %v", name, err)
		}
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
	if _, err = h.belt.Call(ctx, old, read); !errors.Is(err, extension.ErrUnauthorized) || errors.Is(err, extension.ErrAuthorityReleased) {
		t.Fatalf("superseded token accepted, or refused as released: %v", err)
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

// TestProductionToolbeltCancelsChild: an execution cancels child Work it
// asked for. The cancel is recorded on the child, caused by the
// execution's trigger, the child ends cancelled, and the parent hears of it
// like any other end. Work that isn't its child, and a child that has
// already ended, are refused.
func TestProductionToolbeltCancelsChild(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	ctx := context.Background()
	child := domain.Work{Kind: "fixture.child"}
	asked, err := h.belt.CallRequest(ctx, h.token, "child", extension.ToolCall{Operation: extension.CreateWork, Child: &child})
	if err != nil {
		t.Fatal(err)
	}
	id := asked.Work.ID
	// The caller's own emissions can't take the cancel record's place:
	// an Event with the delivery ID the host's record would have.
	for _, delivery := range []string{"cancel/" + string(id), "child-cancel/" + string(id)} {
		squat := domain.Event{Type: "fixture.changed.v1", DeliveryID: delivery, Data: domain.Payload{Schema: "fixture.v1", Data: []byte(`{}`)}}
		if _, err = h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.Emit, Event: &squat}); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []domain.ID{"work/item", "work/unknown"} {
		if _, err = h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.CancelWork, Cancel: &extension.CancelRequest{Work: target}}); !errors.Is(err, extension.ErrUnauthorized) {
			t.Fatalf("cancel of %s, not the caller's child, accepted: %v", target, err)
		}
	}
	if _, err = h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.CancelWork, Cancel: &extension.CancelRequest{Work: id, Reason: strings.Repeat("x", extension.MaxCancelReason+1)}}); !errors.Is(err, extension.ErrInvalid) {
		t.Fatalf("oversized reason accepted: %v", err)
	}
	out, err := h.belt.CallRequest(ctx, h.token, "cancel", extension.ToolCall{Operation: extension.CancelWork, Cancel: &extension.CancelRequest{Work: id, Reason: "never mind"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Work == nil || out.Work.ID != id || out.Work.State != domain.WorkCancelled {
		t.Fatalf("cancelled child = %+v", out.Work)
	}
	cancel := out.Event.Event
	var why domain.ChildCancel
	if err = json.Unmarshal(cancel.Data.Data, &why); err != nil || cancel.Type != domain.TypeChildCancel || cancel.WorkID != id || cancel.CausationID != "trigger" || cancel.Source != domain.LineageSource || why.Execution != string(h.attempt.Lease.ExecutionID) || why.Parent != "work/item" || why.Reason != "never mind" {
		t.Fatalf("cancel recorded as %+v (%+v), %v", cancel, why, err)
	}
	// A retry with the same request ID replays the cancel.
	if again, err := h.belt.CallRequest(ctx, h.token, "cancel", extension.ToolCall{Operation: extension.CancelWork, Cancel: &extension.CancelRequest{Work: id, Reason: "never mind"}}); err != nil || again.Event.Event.ID != cancel.ID {
		t.Fatalf("retried cancel not replayed: %v", err)
	}
	var oe *extension.OperationError
	if _, err = h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.CancelWork, Cancel: &extension.CancelRequest{Work: id}}); !errors.As(err, &oe) || oe.Code != extension.CodeWorkEnded {
		t.Fatalf("cancel of ended child accepted: %v", err)
	}
	err = storage.View(ctx, h.f.Store, h.f.Clock.Now(), func(tx storage.Tx) error {
		ended, e := tx.EventByDelivery(domain.LineageSource, "child-ended/"+string(id))
		if e != nil {
			return e
		}
		var c domain.ChildEnded
		if e = json.Unmarshal(ended.Event.Data.Data, &c); e != nil {
			return e
		}
		if ended.Event.WorkID != "work/item" || c.Work != string(id) || c.State != domain.WorkCancelled {
			t.Fatalf("the parent heard %+v: %+v", ended.Event, c)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestProductionToolbeltRefusesAWaitNoChildCanEnd: a wait only for a
// child's end, declared before the create_work it was for, could never end,
// so the host refuses it without declaring it, and the caller keeps its
// authority to ask for the child and then wait. A wait that names another
// type too, or one declared after create_work or after a child has ended,
// is accepted as before.
func TestProductionToolbeltRefusesAWaitNoChildCanEnd(t *testing.T) {
	ctx := context.Background()
	childEnd := func(types ...string) extension.ToolCall {
		return extension.ToolCall{Operation: extension.Wait, Wait: &extension.WaitRequest{Match: domain.EventMatch{Types: types}}}
	}
	refused := func(t *testing.T, err error) {
		t.Helper()
		var oe *extension.OperationError
		if !errors.As(err, &oe) || oe.Code != runtime.CodeNoChildInProgress || !strings.Contains(oe.Message, "call create_work first, then wait") {
			t.Fatalf("a wait no child can end was not refused: %v", err)
		}
	}
	t.Run("before create_work", func(t *testing.T) {
		h := newToolbeltHarness(t, "persistent")
		_, err := h.belt.Call(ctx, h.token, childEnd(domain.TypeChildEnded))
		refused(t, err)
		_, err = h.belt.Call(ctx, h.token, childEnd(domain.TypeChildEnded, domain.TypeChildReported))
		refused(t, err)
		// Nothing was declared: the turn can still ask for the child, and
		// then wait for it.
		child := domain.Work{Kind: "fixture.child"}
		asked, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.CreateWork, Child: &child})
		if err != nil {
			t.Fatalf("create_work after the refused wait: %v", err)
		}
		out, err := h.belt.Call(ctx, h.token, childEnd(domain.TypeChildEnded))
		if err != nil || out.Wait == nil {
			t.Fatalf("wait after create_work: %v", err)
		}
		if err = h.scheduler.Complete(ctx, h.attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); err != nil {
			t.Fatal(err)
		}
		routingRead(t, h.f, func(tx storage.Tx) error {
			w, e := tx.Work("work/item")
			if e != nil {
				return e
			}
			c, e := tx.Work(asked.Work.ID)
			if e != nil {
				return e
			}
			if w.State != domain.WorkWaiting || c.ParentID != w.ID || c.State.Terminal() {
				t.Fatalf("the parent did not park with its child in progress: parent %s, child %+v", w.State, c)
			}
			return nil
		})
	})
	t.Run("keys leaving out the lineage key", func(t *testing.T) {
		h := newToolbeltHarness(t, "persistent")
		child := domain.Work{Kind: "fixture.child"}
		if _, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.CreateWork, Child: &child}); err != nil {
			t.Fatal(err)
		}
		_, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.Wait, Wait: &extension.WaitRequest{Match: domain.EventMatch{Keys: []domain.CorrelationKey{toolbeltKey}, Types: []string{domain.TypeChildEnded}}}})
		var oe *extension.OperationError
		if !errors.As(err, &oe) || oe.Code != runtime.CodeNoChildInProgress || !strings.Contains(oe.Message, "core.work/work/item") {
			t.Fatalf("a wait for a child's end off the lineage key was not refused: %v", err)
		}
	})
	t.Run("another type too", func(t *testing.T) {
		h := newToolbeltHarness(t, "persistent")
		if out, err := h.belt.Call(ctx, h.token, childEnd(domain.TypeChildEnded, "fixture.changed")); err != nil || out.Wait == nil {
			t.Fatalf("a wait that a message can still end was refused: %v", err)
		}
	})
	t.Run("after the child ended", func(t *testing.T) {
		h := newToolbeltHarness(t, "persistent")
		child := domain.Work{Kind: "fixture.child"}
		asked, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.CreateWork, Child: &child})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.CancelWork, Cancel: &extension.CancelRequest{Work: asked.Work.ID}}); err != nil {
			t.Fatal(err)
		}
		// No child is in progress, and its end can't wake a wait only
		// for reports: that wait is refused, and the refusal leaves the
		// turn its authority.
		_, err = h.belt.Call(ctx, h.token, childEnd(domain.TypeChildReported))
		refused(t, err)
		// Its end has arrived for a wait on ends to wake on.
		if out, err := h.belt.Call(ctx, h.token, childEnd(domain.TypeChildEnded)); err != nil || out.Wait == nil {
			t.Fatalf("a wait on a child's end that has arrived was refused: %v", err)
		}
	})
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
	// The refusal names the release, and still fails closed as unauthorized.
	_, err = h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.ReadContext})
	if !errors.Is(err, extension.ErrAuthorityReleased) || !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("declared wait retained authority: %v", err)
	}
	if _, err = h.belt.Call(ctx, h.token, extension.ToolCall{Operation: "fixture.echo", Input: domain.Payload{Schema: "fixture.v1", Data: json.RawMessage(`{}`)}}); !errors.Is(err, extension.ErrAuthorityReleased) {
		t.Fatalf("extension operation after declared wait: %v", err)
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
	if _, err := h.belt.Call(ctx, h.token, extension.ToolCall{Operation: extension.DeclareOutcome, Outcome: &domain.Outcome{Kind: domain.OutcomeCompleted}}); !errors.Is(err, extension.ErrAuthorityReleased) {
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

// TestProductionToolbeltRecordsConfigDrift: a refused call the host
// classifies (an operation the binding's playbook declares, which config
// doesn't grant) records why, so it reaches the operator as configuration
// drift rather than as the agent reaching outside its role. Negative
// control: an operation outside the grant the host doesn't classify is
// only not_granted, and a granted call carries no reason.
func TestProductionToolbeltRecordsConfigDrift(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	ctx := context.Background()
	// Registered, but not in the attempt's grant.
	noop := func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
		return domain.Payload{}, nil
	}
	if err := h.belt.Register(extension.Registration{Version: extension.ContractVersion, Name: "tracker",
		Operations: []extension.Operation{{Name: "tracker.edit", Handler: noop}, {Name: "tracker.delete", Handler: noop}}}); err != nil {
		t.Fatal(err)
	}
	h.refusal = func(c runtime.AuthorityCheck) string {
		if c.Operation == "tracker.edit" && c.Binding == "fixture" && c.ScopeID == "routing-scope" {
			return runtime.RefusalConfigDrift
		}
		return ""
	}
	for _, op := range []string{"tracker.edit", "tracker.delete", "fixture.echo"} {
		_, _ = h.belt.Call(ctx, h.token, extension.ToolCall{Operation: op, Input: domain.Payload{Schema: "fixture.echo.v1", Data: []byte(`{}`)}})
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	got := map[string]string{}
	for _, r := range *h.records {
		got[r.Attributes["toolbelt.operation"]] = r.Name + " " + r.Attributes["toolbelt.result"] + " " + r.Attributes["toolbelt.refusal"]
	}
	want := map[string]string{
		"tracker.edit":   "toolbelt.rejected unauthorized config_drift",
		"tracker.delete": "toolbelt.rejected unauthorized not_granted",
		"fixture.echo":   "toolbelt.called ok ",
	}
	for op, w := range want {
		if got[op] != w {
			t.Errorf("%s: %q, want %q", op, got[op], w)
		}
	}
}

// TestProductionToolbeltRecordsLapsedLease: a call refused because the
// execution's lease lapsed under it (a host that slept or stalled past the
// lease) records lease_lost, and one with the token recovery then revoked
// records token_ended, so the record says the authority ended rather than
// that the caller reached outside it. A lapsed lease is never configuration
// drift, even for an operation the host would classify as drift. The
// execution is still running, so both refusals are
// ErrAuthorityRecovering: the call did not run and the same call with the
// recovered credential is accepted.
func TestProductionToolbeltRecordsLapsedLease(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	ctx := context.Background()
	h.refusal = func(runtime.AuthorityCheck) string { return runtime.RefusalConfigDrift }
	echo := extension.ToolCall{Operation: "fixture.echo", Input: domain.Payload{Schema: "fixture.echo.v1", Data: []byte(`{}`)}}
	h.f.Clock.Advance(conformance.RuntimeLease + time.Second)
	if _, err := h.belt.Call(ctx, h.token, echo); !errors.Is(err, extension.ErrAuthorityRecovering) {
		t.Fatalf("a call under a lapsed lease: %v, want ErrAuthorityRecovering", err)
	}
	recovered, err := h.scheduler.Recover(ctx, h.attempt.Lease.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.belt.Call(ctx, h.token, echo); !errors.Is(err, extension.ErrAuthorityRecovering) {
		t.Fatalf("a token recovery superseded: %v, want ErrAuthorityRecovering", err)
	}
	if *h.echoes != 0 {
		t.Fatalf("a refused call ran its handler %d times", *h.echoes)
	}
	a := h.authority()
	a.ExecutionID, a.Lease = recovered.Lease.ExecutionID, recovered.Lease
	fresh, err := h.belt.Issue(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.belt.Call(ctx, fresh, echo); err != nil {
		t.Fatalf("the same call with the recovered credential: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var refusals []string
	for _, r := range *h.records {
		if r.Name == "toolbelt.rejected" {
			refusals = append(refusals, r.Attributes["toolbelt.result"]+" "+r.Attributes["toolbelt.refusal"])
		}
	}
	if strings.Join(refusals, ",") != "authority_recovering lease_lost,authority_recovering token_ended" {
		t.Fatalf("refusals %q, want lease_lost for the lapsed lease, then token_ended for the token recovery revoked, both recovering", refusals)
	}
}

// TestProductionToolbeltRecoveringOnlyWhileAdoptable is the negative
// control for ErrAuthorityRecovering: a refusal says "retry" only when the
// call did not run and recovery can still give the process fresh
// authority. A lease that lapses while the handler runs (its effect may
// have happened), a token past its expiry under a live lease, and a token
// whose execution has finished keep the bare ErrUnauthorized.
func TestProductionToolbeltRecoveringOnlyWhileAdoptable(t *testing.T) {
	echo := extension.ToolCall{Operation: "fixture.echo", Input: domain.Payload{Schema: "fixture.echo.v1", Data: []byte(`{}`)}}
	bare := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, extension.ErrUnauthorized) || errors.Is(err, extension.ErrAuthorityRecovering) {
			t.Fatalf("got %v, want the bare ErrUnauthorized", err)
		}
	}
	t.Run("lapsed while the handler ran", func(t *testing.T) {
		h := newToolbeltHarness(t, "persistent")
		h.onEcho = func(string) { h.f.Clock.Advance(conformance.RuntimeLease + time.Second) }
		_, err := h.belt.CallRequest(context.Background(), h.token, "lapse-mid-call", echo)
		bare(t, err)
	})
	t.Run("token expired under a live lease", func(t *testing.T) {
		h := newToolbeltHarness(t, "persistent")
		a := h.authority()
		a.ExpiresAt = h.f.Clock.Now().Add(time.Second)
		short, err := h.belt.Issue(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		h.f.Clock.Advance(2 * time.Second)
		_, err = h.belt.Call(context.Background(), short, echo)
		bare(t, err)
	})
	t.Run("execution finished", func(t *testing.T) {
		h := newToolbeltHarness(t, "persistent")
		ctx := context.Background()
		if err := h.scheduler.Complete(ctx, h.attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); err != nil {
			t.Fatal(err)
		}
		h.f.Clock.Advance(conformance.RuntimeLease + time.Second)
		_, err := h.belt.Call(ctx, h.token, echo)
		bare(t, err)
	})
}

// TestProductionToolbeltGateSkipsItsOwnOperations: a gate never decides on
// its own registration's operations.
func TestProductionToolbeltGateSkipsItsOwnOperations(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	ctx := context.Background()
	var asked []string
	belt, err := runtime.NewToolbelt(h.f.Store, h.f.Clock.Now, h.f.Driver.(*schedulerDriver).Router, runtime.ToolbeltOptions{})
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

// TestProductionToolbeltSweepDropsDeadRecords: once an execution has finished
// and its token is revoked, the retention pass's toolbelt sweep deletes the
// token and the execution's replay records after the grace. Within the grace,
// and while the execution runs, nothing is deleted (the controls), and the
// swept token is refused like the revoked one.
func TestProductionToolbeltSweepDropsDeadRecords(t *testing.T) {
	h := newToolbeltHarness(t, "persistent")
	ctx := context.Background()
	in := domain.Payload{Schema: "fixture.echo.v1", Data: []byte(`{"x":1}`)}
	if _, err := h.belt.CallRequest(ctx, h.token, "kept", extension.ToolCall{Operation: "fixture.echo", Input: in}); err != nil {
		t.Fatal(err)
	}
	h.f.Clock.Advance(time.Second)
	if got, err := h.belt.SweepToolbelt(ctx, 0, 100); err != nil || got != (runtime.ToolbeltSweep{}) {
		t.Fatalf("swept a running execution's records: %+v %v", got, err)
	}
	if err := h.scheduler.Complete(ctx, h.attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); err != nil {
		t.Fatal(err)
	}
	if got, err := h.belt.SweepToolbelt(ctx, time.Hour, 100); err != nil || got != (runtime.ToolbeltSweep{}) {
		t.Fatalf("swept records within the grace: %+v %v", got, err)
	}
	h.f.Clock.Advance(2 * time.Hour)
	if got, err := h.belt.SweepToolbelt(ctx, time.Hour, 100); err != nil || got.Tokens < 1 || got.Replays != 1 {
		t.Fatalf("dead records kept past the grace: %+v %v", got, err)
	}
	if known, err := h.belt.Known(ctx, h.token); err != nil || known {
		t.Fatalf("swept token still known: %v %v", known, err)
	}
	if _, err := h.belt.CallRequest(ctx, h.token, "kept", extension.ToolCall{Operation: "fixture.echo", Input: in}); !errors.Is(err, extension.ErrUnauthorized) {
		t.Fatalf("swept token accepted: %v", err)
	}
}
