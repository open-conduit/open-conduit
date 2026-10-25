package conformance

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/observability"
)

// IngressFixture supplies a service-specific fixture with two versions of one
// item, two complete scan pages, and a declared one-shot push event.
type IngressFixture struct {
	Ingress     extension.Ingress
	Source      extension.Source
	Delivery    extension.Delivery
	Canonical   extension.Observation
	OneShotType string
	// OneShot, when set, is a separate delivery yielding the one-shot event,
	// for services whose one-shot family never accompanies an observation
	// (Slack's push-only deletions). Delivery then yields only Canonical.
	OneShot *extension.Delivery
}

func CheckIngress(f IngressFixture) error {
	ctx := context.Background()
	families := map[string]extension.EventFamily{}
	for _, family := range f.Ingress.Spec().Families {
		if family.Type == "" || !family.ReDerivable && family.Loss == "" {
			return fmt.Errorf("missing explicit loss limitation")
		}
		if _, ok := families[family.Type]; ok {
			return fmt.Errorf("duplicate event family")
		}
		families[family.Type] = family
	}
	one, ok := families[f.OneShotType]
	if !ok || one.ReDerivable || one.Loss == "" {
		return fmt.Errorf("one-shot loss limitation missing")
	}
	push, err := f.Ingress.Push(ctx, f.Source, f.Delivery)
	if err != nil {
		return err
	}
	retry, err := f.Ingress.Push(ctx, f.Source, f.Delivery)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(push, retry) {
		return fmt.Errorf("push normalization is unstable")
	}
	if len(push.Observations) != 1 || !reflect.DeepEqual(push.Observations[0], f.Canonical) {
		return fmt.Errorf("push canonical observation mismatch")
	}
	oneShot := push
	if f.OneShot != nil {
		if len(push.Events) != 0 {
			return fmt.Errorf("canonical delivery carries unexpected one-shot events")
		}
		if oneShot, err = f.Ingress.Push(ctx, f.Source, *f.OneShot); err != nil {
			return err
		}
		if len(oneShot.Observations) != 0 {
			return fmt.Errorf("one-shot delivery carries observations")
		}
	}
	if len(oneShot.Events) != 1 || oneShot.Events[0].Type != f.OneShotType {
		return fmt.Errorf("one-shot fixture missing")
	}
	for _, e := range oneShot.Events {
		if family, ok := families[e.Type]; !ok || family.ReDerivable {
			return fmt.Errorf("undeclared non-re-derivable event")
		}
	}
	var all []extension.Observation
	scan := extension.Scan{Source: f.Source}
	seen := map[string]bool{}
	for n := 0; n < 10; n++ {
		page, err := f.Ingress.Reconcile(ctx, scan)
		if err != nil {
			return err
		}
		if page.ScanID == "" || scan.ScanID != "" && scan.ScanID != page.ScanID || page.Complete && page.NextCursor != "" {
			return fmt.Errorf("invalid completeness/generation")
		}
		all = append(all, page.Observations...)
		if page.NextCursor == "" {
			if !page.Complete {
				return fmt.Errorf("fixture scan incomplete")
			}
			found := false
			for _, o := range all {
				family, declared := families[o.Event.Type]
				if !declared || !family.ReDerivable || o.ID == "" || o.Version == "" || o.Event.Source != f.Source.Namespace || o.Event.DeliveryID == "" {
					return fmt.Errorf("invalid observation identity/family")
				}
				if o.ID == f.Canonical.ID {
					found = reflect.DeepEqual(o, f.Canonical)
				}
			}
			if !found {
				return fmt.Errorf("push/reconcile canonical identity differs")
			}
			return nil
		}
		if page.Complete || seen[page.NextCursor] {
			return fmt.Errorf("pagination repeats or completes early")
		}
		seen[page.NextCursor] = true
		scan.ScanID, scan.Cursor, scan.Checkpoint = page.ScanID, page.NextCursor, page.Checkpoint
	}
	return fmt.Errorf("scan did not finish")
}
func RunIngress(t *testing.T, factory func() IngressFixture) {
	t.Helper()
	if err := CheckIngress(factory()); err != nil {
		t.Fatal(err)
	}
}

func CheckExecutor(e extension.Executor, r extension.RunRequest) error {
	if err := extension.ValidateBinding(domain.Binding{}, e); err != nil {
		return err
	}
	persisted := false
	var state domain.Payload
	r.PersistRecovery = func(s domain.Payload) error { persisted = true; state = s; return nil }
	h, err := e.Run(context.Background(), r)
	if err != nil {
		return err
	}
	if h == nil || !persisted {
		return fmt.Errorf("run acknowledged before recovery metadata")
	}
	result, err := h.Wait(context.Background())
	if err != nil {
		return err
	}
	if err := extension.ValidateResult(result); err != nil {
		return err
	}
	// Recovery must not call Run or overwrite metadata and must keep the outcome.
	// It may re-acknowledge the identical metadata under its new lease, which
	// the production scheduler requires before accepting an adopted handle.
	r.PersistRecovery = func(p domain.Payload) error {
		if !reflect.DeepEqual(p, state) {
			return fmt.Errorf("recovery launched replacement")
		}
		return nil
	}
	adopted, err := e.Recover(context.Background(), r, state)
	if err != nil {
		return err
	}
	if adopted == nil {
		return fmt.Errorf("recovery returned no handle")
	}
	recovered, err := adopted.Wait(context.Background())
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(result, recovered) {
		return fmt.Errorf("recovery changed result")
	}
	if _, err := e.Recover(context.Background(), r, domain.Payload{}); !errors.Is(err, extension.ErrNotAdoptable) {
		return fmt.Errorf("unknown recovery state accepted: %v", err)
	}
	stop := errors.New("persistence failed")
	r.PersistRecovery = func(domain.Payload) error { return stop }
	if _, err := e.Run(context.Background(), r); !errors.Is(err, stop) {
		return fmt.Errorf("recovery persistence failure ignored: %v", err)
	}
	return nil
}
func RunExecutor(t *testing.T, factory func() extension.Executor, r extension.RunRequest) {
	t.Helper()
	if err := CheckExecutor(factory(), r); err != nil {
		t.Fatal(err)
	}
}

// InjectFixture drives an inject-capable executor through the generic inject
// acknowledgement contract (B04, 04-02). Request starts an attempt that stays
// running until Finish. Ready returns once the running attempt can take input.
// Finish lets the attempt take in everything injected so far and finish (for
// example a fake process reading its inbox, or a fake claude echoing).
type InjectFixture struct {
	Executor extension.Executor
	Request  extension.RunRequest
	Ready    func() error
	Finish   func() error
}

// CheckInjectExecutor verifies, for an executor that advertises inject:
//   - Inject to an execution the executor does not hold is ErrNotAccepting;
//   - Inject to the running attempt is accepted (nil), and delivering the same
//     Event identity again (a retried ambiguous dispatch) is accepted too;
//   - the finished result proves consumption of what the attempt took in, at
//     most once per Event and never naming an Event it was not given;
//   - after the result, Inject is ErrNotAccepting (nothing can take input);
//   - Recover returns the identical proof (it is durable executor state).
func CheckInjectExecutor(f InjectFixture) error {
	e := f.Executor
	if !e.Capabilities().Inject {
		return fmt.Errorf("executor does not advertise inject")
	}
	if err := extension.ValidateBinding(domain.Binding{Name: "inject", Requires: domain.Capabilities{Inject: true}}, e); err != nil {
		return err
	}
	injector := e.(extension.InjectExecutor)
	ctx := context.Background()
	r := f.Request
	var state domain.Payload
	r.PersistRecovery = func(s domain.Payload) error { state = s; return nil }
	h, err := e.Run(ctx, r)
	if err != nil {
		return err
	}
	if f.Ready != nil {
		if err := f.Ready(); err != nil {
			return err
		}
	}
	first := domain.Event{ID: "inject-conformance-first", Type: "fixture.message", Source: "fixture", DeliveryID: "first", Data: domain.Payload{Schema: "fixture.message.v1", Data: []byte(`{"text":"also this"}`)}}
	if err := injector.Inject(ctx, "inject-conformance-unknown", first); !errors.Is(err, extension.ErrNotAccepting) {
		return fmt.Errorf("inject to an unknown execution: %v, want ErrNotAccepting", err)
	}
	if err := injector.Inject(ctx, r.Execution.ID, first); err != nil {
		return fmt.Errorf("inject to the running attempt: %w", err)
	}
	if err := injector.Inject(ctx, r.Execution.ID, first); err != nil {
		return fmt.Errorf("retried inject of the same event: %w", err)
	}
	if f.Finish != nil {
		if err := f.Finish(); err != nil {
			return err
		}
	}
	result, err := h.Wait(ctx)
	if err != nil {
		return err
	}
	if err := extension.ValidateResult(result); err != nil {
		return err
	}
	if len(result.ConsumedInputs) != 1 || result.ConsumedInputs[0] != first.ID {
		return fmt.Errorf("consumption proof %v, want exactly [%s]", result.ConsumedInputs, first.ID)
	}
	late := domain.Event{ID: "inject-conformance-late", Type: "fixture.message", Source: "fixture", DeliveryID: "late"}
	if err := injector.Inject(ctx, r.Execution.ID, late); !errors.Is(err, extension.ErrNotAccepting) {
		return fmt.Errorf("inject after the result: %v, want ErrNotAccepting", err)
	}
	r.PersistRecovery = func(p domain.Payload) error {
		if !reflect.DeepEqual(p, state) {
			return fmt.Errorf("recovery launched replacement")
		}
		return nil
	}
	adopted, err := e.Recover(ctx, r, state)
	if err != nil {
		return err
	}
	recovered, err := adopted.Wait(ctx)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(recovered.ConsumedInputs, result.ConsumedInputs) {
		return fmt.Errorf("recovery changed the consumption proof: %v vs %v", recovered.ConsumedInputs, result.ConsumedInputs)
	}
	return nil
}

// RunInjectExecutor runs CheckInjectExecutor as a test.
func RunInjectExecutor(t *testing.T, factory func() InjectFixture) {
	t.Helper()
	if err := CheckInjectExecutor(factory()); err != nil {
		t.Fatal(err)
	}
}

// ToolbeltFixture exposes only public interfaces plus trusted test controls.
// Factory starts a running execution and issues a token for all five builtins
// and fixture.echo; fixture.changed.v1 is its declared emitted event family.
type ToolbeltFixture struct {
	Belt      extension.Toolbelt
	Issuer    extension.TokenIssuer
	Registrar extension.Registrar
	Token     string
	Authority extension.Authority
	Advance   func(time.Duration)
	LoseLease func() error
}
type ToolbeltCase struct {
	Name  string
	Check func(ToolbeltFixture) error
}

func ToolbeltCases() []ToolbeltCase {
	return []ToolbeltCase{
		{"cross_scope_work_execution", func(f ToolbeltFixture) error {
			for _, c := range []extension.ToolCall{{Operation: extension.ReadContext, ScopeID: "foreign"}, {Operation: extension.ReadContext, WorkID: "foreign"}, {Operation: extension.ReadContext, ExecutionID: "foreign"}} {
				if _, err := f.Belt.Call(context.Background(), f.Token, c); !errors.Is(err, extension.ErrUnauthorized) {
					return fmt.Errorf("cross-target authority accepted: %v", err)
				}
			}
			return nil
		}},
		{"expiry_revocation_and_lease", func(f ToolbeltFixture) error {
			ctx := context.Background()
			c := extension.ToolCall{Operation: extension.ReadContext}
			if _, err := f.Belt.Call(ctx, f.Token, c); err != nil {
				return err
			}
			if err := f.Issuer.Revoke(ctx, f.Token); err != nil {
				return err
			}
			if _, err := f.Belt.Call(ctx, f.Token, c); !errors.Is(err, extension.ErrUnauthorized) {
				return fmt.Errorf("revoked token accepted: %v", err)
			}
			token, err := f.Issuer.Issue(ctx, f.Authority)
			if err != nil {
				return err
			}
			f.Advance(time.Minute)
			if _, err := f.Belt.Call(ctx, token, c); !errors.Is(err, extension.ErrUnauthorized) {
				return fmt.Errorf("expired token accepted: %v", err)
			}
			return nil
		}},
		{"stale_lease", func(f ToolbeltFixture) error {
			if err := f.LoseLease(); err != nil {
				return err
			}
			if _, err := f.Belt.Call(context.Background(), f.Token, extension.ToolCall{Operation: extension.ReadContext}); !errors.Is(err, extension.ErrUnauthorized) {
				return fmt.Errorf("stale lease accepted: %v", err)
			}
			return nil
		}},
		{"registration_and_allowlist", func(f ToolbeltFixture) error {
			ctx := context.Background()
			if _, err := f.Belt.Call(ctx, f.Token, extension.ToolCall{Operation: "missing"}); !errors.Is(err, extension.ErrUndeclared) {
				return fmt.Errorf("undeclared operation accepted: %v", err)
			}
			for _, op := range []string{extension.Emit, "fixture.echo"} {
				if err := f.Registrar.Register(extension.Registration{Version: extension.ContractVersion, Name: "conflict." + op, Operations: []extension.Operation{{Name: op, Handler: func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
					return domain.Payload{}, nil
				}}}}); !errors.Is(err, extension.ErrConflict) {
					return fmt.Errorf("conflicting operation accepted: %v", err)
				}
			}
			a := f.Authority
			a.Operations = []string{extension.ReadContext}
			token, err := f.Issuer.Issue(ctx, a)
			if err != nil {
				return err
			}
			if _, err := f.Belt.Call(ctx, token, extension.ToolCall{Operation: "fixture.echo"}); !errors.Is(err, extension.ErrUnauthorized) {
				return fmt.Errorf("operation allowlist ignored: %v", err)
			}
			return nil
		}},
		{"emission_causation", func(f ToolbeltFixture) error {
			e := domain.Event{ID: "emission", Type: "fixture.changed.v1", DeliveryID: "report", OccurredAt: NewClock().Now()}
			out, err := f.Belt.Call(context.Background(), f.Token, extension.ToolCall{Operation: extension.Emit, Event: &e})
			if err != nil {
				return err
			}
			if out.Event == nil {
				return fmt.Errorf("no accepted emission")
			}
			saved := out.Event.Event
			if saved.CausationID != f.Authority.TriggerID || saved.WorkID != f.Authority.WorkID || saved.ExecutionID != f.Authority.ExecutionID {
				return fmt.Errorf("emission lost causation/authority")
			}
			e.ID = "forged"
			e.CausationID = "foreign"
			if _, err := f.Belt.Call(context.Background(), f.Token, extension.ToolCall{Operation: extension.Emit, Event: &e}); !errors.Is(err, extension.ErrUnauthorized) {
				return fmt.Errorf("forged causation accepted: %v", err)
			}
			e.CausationID = ""
			e.Type = "undeclared"
			if _, err := f.Belt.Call(context.Background(), f.Token, extension.ToolCall{Operation: extension.Emit, Event: &e}); !errors.Is(err, extension.ErrUndeclared) {
				return fmt.Errorf("undeclared event accepted: %v", err)
			}
			return nil
		}},
		{"child_and_extension_operation", func(f ToolbeltFixture) error {
			ctx := context.Background()
			child := domain.Work{ID: "child", Kind: "fixture.child", ScopeID: "foreign"}
			if _, err := f.Belt.Call(ctx, f.Token, extension.ToolCall{Operation: extension.CreateWork, Child: &child}); !errors.Is(err, extension.ErrUnauthorized) {
				return fmt.Errorf("cross-scope child accepted: %v", err)
			}
			child.ScopeID = ""
			out, err := f.Belt.Call(ctx, f.Token, extension.ToolCall{Operation: extension.CreateWork, Child: &child})
			if err != nil {
				return err
			}
			if out.Event == nil || out.Event.Event.CausationID != f.Authority.TriggerID {
				return fmt.Errorf("child lost causation")
			}
			if out.Work == nil || out.Work.ParentID != f.Authority.WorkID || out.Work.ScopeID != f.Authority.ScopeID {
				return fmt.Errorf("child lost parent authority")
			}
			input := domain.Payload{Schema: "fixture.echo.v1", Data: []byte(`{"ok":true}`)}
			out, err = f.Belt.Call(ctx, f.Token, extension.ToolCall{Operation: "fixture.echo", Input: input})
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(input, out.Output) {
				return fmt.Errorf("extension operation lost payload")
			}
			return nil
		}},
		{"wait_releases_authority", func(f ToolbeltFixture) error {
			timeout := NewClock().Now().Add(time.Hour)
			out, err := f.Belt.Call(context.Background(), f.Token, extension.ToolCall{Operation: extension.Wait, Wait: &extension.WaitRequest{Match: domain.EventMatch{Keys: []domain.CorrelationKey{{Source: "fixture", ExternalID: "item"}}, Types: []string{"fixture.changed.v1"}}, Timeout: &timeout}})
			if err != nil {
				return err
			}
			if out.Wait == nil || out.Wait.TimeoutTimerID == "" {
				return fmt.Errorf("wait missing durable timeout")
			}
			if _, err := f.Belt.Call(context.Background(), f.Token, extension.ToolCall{Operation: extension.ReadContext}); !errors.Is(err, extension.ErrUnauthorized) {
				return fmt.Errorf("waiting execution retained authority: %v", err)
			}
			return nil
		}},
		{"outcome_releases_authority", func(f ToolbeltFixture) error {
			_, err := f.Belt.Call(context.Background(), f.Token, extension.ToolCall{Operation: extension.DeclareOutcome, Outcome: &domain.Outcome{Kind: domain.OutcomeCompleted}})
			if err != nil {
				return err
			}
			if _, err := f.Belt.Call(context.Background(), f.Token, extension.ToolCall{Operation: extension.ReadContext}); !errors.Is(err, extension.ErrUnauthorized) {
				return fmt.Errorf("terminal execution retained authority: %v", err)
			}
			return nil
		}},
	}
}
func RunToolbelt(t *testing.T, factory func() ToolbeltFixture) {
	t.Helper()
	for _, c := range ToolbeltCases() {
		t.Run(c.Name, func(t *testing.T) {
			if err := c.Check(factory()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// CheckObserver verifies boundary validation and redaction without an exporter.
func CheckObserver(o observability.Observer, records func() []observability.Record) error {
	ctx := context.Background()
	valid := observability.Record{Name: "execution.completed", At: NewClock().Now(), Correlation: observability.Correlation{WorkID: "work", EventID: "event", ExecutionID: "execution", ScopeID: "scope", ExecutorBinding: "shell", TraceID: "trace"}, Attributes: map[string]string{"message": "bearer secret-value", "OS_TOKEN": "unknown-value"}}
	cost := 0.25
	tokens := uint64(42)
	valid.Usage = &observability.Measurement{InputTokens: &tokens, Cost: &cost, Currency: "USD"}
	for _, field := range []string{"work", "event", "execution", "scope", "binding", "trace"} {
		bad := valid
		switch field {
		case "work":
			bad.Correlation.WorkID = ""
		case "event":
			bad.Correlation.EventID = ""
		case "execution":
			bad.Correlation.ExecutionID = ""
		case "scope":
			bad.Correlation.ScopeID = ""
		case "binding":
			bad.Correlation.ExecutorBinding = ""
		case "trace":
			bad.Correlation.TraceID = ""
		}
		if err := o.Observe(ctx, bad); err == nil {
			return fmt.Errorf("missing %s correlation accepted", field)
		}
	}
	// A scan record precedes any Event; it must be anchored by its source
	// rather than accepted with neither an Event nor a source.
	unanchored := observability.Record{Name: "ingress.scan_failed", At: valid.At, Correlation: observability.Correlation{ScopeID: "scope", TraceID: "trace"}, Attributes: map[string]string{}}
	if err := o.Observe(ctx, unanchored); err == nil {
		return fmt.Errorf("unanchored ingress record accepted")
	}
	if err := o.Observe(ctx, valid); err != nil {
		return err
	}
	got := records()
	if len(got) != 1 {
		return fmt.Errorf("invalid records exported")
	}
	if got[0].Attributes["message"] != "bearer [REDACTED]" || got[0].Attributes["OS_TOKEN"] != "[REDACTED]" {
		return fmt.Errorf("secret escaped redaction")
	}
	if !reflect.DeepEqual(got[0].Correlation, valid.Correlation) {
		return fmt.Errorf("telemetry lost correlation")
	}
	if got[0].Usage == nil || got[0].Usage.InputTokens == nil || *got[0].Usage.InputTokens != tokens || got[0].Usage.Cost == nil || *got[0].Usage.Cost != cost || got[0].Usage.Currency != "USD" {
		return fmt.Errorf("executor usage/cost missing")
	}
	if valid.Attributes["message"] != "bearer secret-value" {
		return fmt.Errorf("redaction mutated caller")
	}
	return nil
}
func RunObserver(t *testing.T, o observability.Observer, records func() []observability.Record) {
	t.Helper()
	if err := CheckObserver(o, records); err != nil {
		t.Fatal(err)
	}
}
