package inprocsdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
)

// acme is an in-process extension with a source per entry (a webhook for
// those that ask for one), one operation it registers only once started, a
// task and a check.
type acme struct {
	mu     sync.Mutex
	host   *inproc.Host
	starts int
	// failStarts is how many Starts fail before one succeeds.
	failStarts int
	entries    []protocol.ConfigEntry
	ran        []string
}

var (
	_ inproc.Starter    = (*acme)(nil)
	_ inproc.Configurer = (*acme)(nil)
	_ inproc.Ingester   = (*acme)(nil)
	_ inproc.Webhooker  = (*acme)(nil)
	_ inproc.Operator   = (*acme)(nil)
	_ inproc.Tooled     = (*acme)(nil)
	_ inproc.Tasker     = (*acme)(nil)
	_ inproc.Checker    = (*acme)(nil)
	_ inproc.Typed      = (*acme)(nil)
)

func (*acme) Name() string { return "acme" }

func (x *acme) Types() []inproc.ConfigType {
	return []inproc.ConfigType{{Kind: protocol.ValidateExtension, Type: "acme", Ingress: true, Validate: func(c json.RawMessage, _ []string) error {
		if strings.Contains(string(c), "bad") {
			return extension.ErrInvalid
		}
		return nil
	}}}
}

func (x *acme) Start(_ context.Context, h inproc.Host) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.starts++
	if x.failStarts > 0 {
		x.failStarts--
		return errors.New("no credentials yet")
	}
	x.host = &h
	return nil
}

func (x *acme) Configure(_ context.Context, p protocol.ConfigureParams) {
	x.mu.Lock()
	x.entries = p.Entries
	x.mu.Unlock()
}

type ingress struct{}

func (ingress) Spec() extension.IngressSpec {
	return extension.IngressSpec{Families: []extension.EventFamily{{Type: "acme.thing", ReDerivable: true}}}
}
func (ingress) Push(context.Context, extension.Source, extension.Delivery) (extension.PushResult, error) {
	return extension.PushResult{}, nil
}
func (ingress) Reconcile(context.Context, extension.Scan) (extension.Snapshot, error) {
	return extension.Snapshot{Complete: true}, nil
}

func (x *acme) Sources() []inproc.Source {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []inproc.Source
	for _, e := range x.entries {
		out = append(out, inproc.Source{Name: "acme/" + e.ScopeID + "/" + e.Name, Source: extension.Source{Namespace: "acme/" + e.Name, ScopeID: domain.ID(e.ScopeID)}, Ingress: ingress{}, Interval: time.Minute})
	}
	return out
}

// Webhook serves an entry whose name ends -hook.
func (x *acme) Webhook(path string) http.Handler {
	if !strings.HasSuffix(path, "-hook") {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
}

func (*acme) Operations() []string { return []string{"acme.ping"} }

func (x *acme) Registration() extension.Registration {
	x.mu.Lock()
	started := x.host != nil
	x.mu.Unlock()
	if !started {
		return extension.Registration{}
	}
	return extension.Registration{Version: extension.ContractVersion, Name: "acme", Operations: []extension.Operation{{Name: "acme.ping",
		Handler: func(context.Context, extension.Authority, domain.Payload) (domain.Payload, error) {
			return domain.Payload{Schema: "acme.pong.v1", Data: json.RawMessage(`{"pong":true}`)}, nil
		}}}}
}

func (x *acme) Tasks() []inproc.Task {
	return []inproc.Task{{Name: "sweep_things", Pace: 30 * time.Second, Run: func(context.Context) error {
		x.mu.Lock()
		x.ran = append(x.ran, "sweep")
		x.mu.Unlock()
		return nil
	}}}
}

func (*acme) Checks() []inproc.Check {
	return []inproc.Check{{Run: func(context.Context, protocol.CheckConfig) inproc.CheckResult {
		return inproc.CheckResult{Name: "acme", Status: inproc.CheckWarn, Code: "half_set_up", Items: []string{"one entry"}}
	}}}
}

func base() protocol.Manifest {
	return protocol.Manifest{Version: "0.1.0", Command: []string{"acme-extension"}, Provides: protocol.Provides{
		Families:   []protocol.Family{{Type: "acme.thing", ReDerivable: true}},
		Schemas:    []protocol.SchemaDecl{{Name: "acme.pong", Versions: []int{1}}},
		Operations: []protocol.OperationDecl{{Name: "acme.ping", Kind: protocol.OperationHandler}},
		Checks:     []protocol.CheckDecl{{Name: "acme"}},
	}}
}

// The bridge needs from the base manifest what the surfaces can't say.
func TestNewRefusesAnIncompleteBase(t *testing.T) {
	b := base()
	b.Provides.Operations = nil
	if _, err := inprocsdk.New(&acme{}, b); err == nil || !strings.Contains(err.Error(), "acme.ping") {
		t.Fatalf("an operation without a kind: %v", err)
	}
	b = base()
	b.Provides.Checks = nil
	if _, err := inprocsdk.New(&acme{}, b); err == nil {
		t.Fatal("unnamed checks accepted")
	}
	b = base()
	b.Name = "other"
	if _, err := inprocsdk.New(&acme{}, b); err == nil {
		t.Fatal("a base manifest of another name accepted")
	}
}

// The manifest takes what the surfaces say, with tasks named after the
// package; the extension starts once, at the first configure; its sources
// and their webhook paths come back from configure; its operation, task,
// check and config validation are served.
func TestBridgeServesTheInprocContract(t *testing.T) {
	x := &acme{}
	b, err := inprocsdk.New(x, base())
	if err != nil {
		t.Fatal(err)
	}
	m := b.Extension().Manifest
	if m.Name != "acme" || !m.Provides.Webhooks || len(m.Provides.Extensions) != 1 || !m.Provides.Extensions[0].Ingress {
		t.Fatalf("manifest %+v", m.Provides)
	}
	if len(m.Provides.Tasks) != 1 || m.Provides.Tasks[0].Name != "acme-sweep-things" || m.Provides.Tasks[0].IntervalSeconds != 30 {
		t.Fatalf("tasks %+v", m.Provides.Tasks)
	}
	ctx := context.Background()
	h := sdktest.New(t, b.Extension())

	// Before the first configure nothing has started, and the operation
	// isn't registered yet.
	if _, err := h.Operation(ctx, "acme.ping", domain.Payload{Data: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("an operation answered before the extension started")
	}
	entries := []protocol.ConfigEntry{{ScopeID: "eng", Type: "acme", Name: "plain", Config: json.RawMessage(`{}`)}, {ScopeID: "eng", Type: "acme", Name: "with-hook", Config: json.RawMessage(`{}`)}}
	for range 2 {
		res, err := h.Configure(ctx, protocol.ConfigureParams{Entries: entries})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, s := range res.Sources {
			names = append(names, s.Name)
			if len(s.Families) != 1 || s.Families[0].Type != "acme.thing" || s.IntervalSeconds != 60 {
				t.Fatalf("source %+v", s)
			}
		}
		if !slices.Equal(names, []string{"acme/eng/plain", "acme/eng/with-hook"}) || !slices.Equal(res.WebhookPaths, []string{"/acme/eng/with-hook"}) {
			t.Fatalf("sources %v webhook paths %v", names, res.WebhookPaths)
		}
	}
	x.mu.Lock()
	starts, host := x.starts, x.host
	x.mu.Unlock()
	if starts != 1 || host == nil || host.Store == nil || host.Secrets == nil || host.Deliver == nil || host.Authorize == nil || host.WaitTarget == nil {
		t.Fatalf("started %d times with host %+v", starts, host)
	}
	out, err := h.Operation(ctx, "acme.ping", domain.Payload{Data: json.RawMessage(`{}`)})
	if err != nil || out.Output.Schema != "acme.pong.v1" {
		t.Fatalf("operation: %+v %v", out, err)
	}
	if err := h.RunTask(ctx, "acme-sweep-things"); err != nil {
		t.Fatal(err)
	}
	x.mu.Lock()
	ran := slices.Clone(x.ran)
	x.mu.Unlock()
	if !slices.Equal(ran, []string{"sweep"}) {
		t.Fatalf("task ran %v", ran)
	}
	c, err := h.Check(ctx, "acme")
	if err != nil || c.OK || c.Status != inproc.CheckWarn || c.Code != "half_set_up" || !slices.Equal(c.Items, []string{"one entry"}) {
		t.Fatalf("check %+v %v", c, err)
	}
	var v protocol.ValidateResult
	if err := h.Call(ctx, "validate", protocol.ValidateParams{Kind: protocol.ValidateExtension, Type: "acme", Config: json.RawMessage(`{"bad":true}`)}, &v); err == nil {
		t.Fatal("a bad entry validated")
	}
	if err := h.Call(ctx, "validate", protocol.ValidateParams{Kind: protocol.ValidateExtension, Type: "acme", Config: json.RawMessage(`{}`)}, &v); err != nil {
		t.Fatalf("a good entry refused: %v", err)
	}
}

// A failed Start fails its configure and is tried again by the next one:
// the extension is never configured, registered or served unstarted.
func TestFailedStartIsRetried(t *testing.T) {
	x := &acme{failStarts: 1}
	b, err := inprocsdk.New(x, base())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h := sdktest.New(t, b.Extension())
	entries := []protocol.ConfigEntry{{ScopeID: "eng", Type: "acme", Name: "plain", Config: json.RawMessage(`{}`)}}
	if _, err := h.Configure(ctx, protocol.ConfigureParams{Entries: entries}); err == nil {
		t.Fatal("a configure whose Start failed succeeded")
	}
	x.mu.Lock()
	configured := len(x.entries)
	x.mu.Unlock()
	if configured != 0 {
		t.Fatal("an unstarted extension was configured")
	}
	if _, err := h.Operation(ctx, "acme.ping", domain.Payload{Data: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("an unstarted extension's operation answered")
	}
	res, err := h.Configure(ctx, protocol.ConfigureParams{Entries: entries})
	if err != nil || len(res.Sources) != 1 {
		t.Fatalf("the retried configure: %+v %v", res, err)
	}
	x.mu.Lock()
	starts := x.starts
	x.mu.Unlock()
	if starts != 2 {
		t.Fatalf("started %d times, want 2", starts)
	}
	if _, err := h.Operation(ctx, "acme.ping", domain.Payload{Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("operation after the retried start: %v", err)
	}
}
