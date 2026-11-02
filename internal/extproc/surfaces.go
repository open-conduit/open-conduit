package extproc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
)

// Surfaces adapted in 07-03: validation, webhooks, resource providers,
// telemetry export, annotations and setup. Each presents the process through
// the contract the host already drives for in-process extensions.

// ValidateTimeout bounds one validate call. Validation runs while the host
// checks a config candidate, so a hung extension refuses the candidate
// rather than stalling it.
const ValidateTimeout = 5 * time.Second

// ErrInvalidConfig is the refusal a validator returns; the host never echoes
// the extension's message into diagnostics.
var ErrInvalidConfig = errors.New("extproc: the extension refused the config object")

// Validator returns the config.Registry validator for one declared type of
// kind protocol.Validate*. A refusal or a failed call (the process is down
// or slow) refuses the object: an unvalidated config is never accepted.
func (r *Remote) Validator(kind, typ string) func(json.RawMessage, []string) error {
	return func(config json.RawMessage, secretNames []string) error {
		ctx, cancel := context.WithTimeout(context.Background(), ValidateTimeout)
		defer cancel()
		if err := r.p.Call(ctx, "validate", protocol.ValidateParams{Kind: kind, Type: typ, Config: config, SecretNames: secretNames}, nil); err != nil {
			return ErrInvalidConfig
		}
		return nil
	}
}

// BindingCheck returns the binding check of a route handler declared with
// binding: validate with kind binding. A refusal's message is the
// package's reason, capped, which the binding_refused diagnostic shows; a
// failed call is a refusal too, so an unreachable package never passes a
// binding it was asked to check.
func (r *Remote) BindingCheck(handler string) func(extension.RouteBinding) error {
	return func(b extension.RouteBinding) error {
		ctx, cancel := context.WithTimeout(context.Background(), ValidateTimeout)
		defer cancel()
		err := r.p.Call(ctx, "validate", protocol.ValidateParams{Kind: protocol.ValidateBinding, Type: handler, Binding: protocol.WireBinding(b)}, nil)
		if err == nil {
			return nil
		}
		if msg, ok := refusal(err); ok {
			return errors.New(msg)
		}
		return errors.New("the extension could not check the binding")
	}
}

// ResourceModeDefault returns the mode a declared resource type's config
// names as its default: validate with kind resource_mode. A failed call
// names none, so the mode warning it feeds is left out rather than wrong.
func (r *Remote) ResourceModeDefault(typ string) func(json.RawMessage) string {
	return func(config json.RawMessage) string {
		return r.mode(protocol.ValidateParams{Kind: protocol.ValidateResourceMode, Type: typ, Config: config})
	}
}

// ExecutorResourceMode returns the resource mode a declared executor
// requests with a binding config: validate with kind executor_mode, "" for
// the resource config's default or a failed call.
func (r *Remote) ExecutorResourceMode(name string) func(json.RawMessage) string {
	return func(config json.RawMessage) string {
		return r.mode(protocol.ValidateParams{Kind: protocol.ValidateExecutorMode, Type: name, Config: config})
	}
}

// ExecutorWarnings returns the config warnings of a declared executor:
// validate with kind executor, whose answer may carry warnings. A failed
// call or a refusal gives none (the config check reports a refusal), and
// each warning is capped to one line.
func (r *Remote) ExecutorWarnings(name string) func(json.RawMessage) []string {
	return func(config json.RawMessage) []string {
		ctx, cancel := context.WithTimeout(context.Background(), ValidateTimeout)
		defer cancel()
		var out protocol.ValidateResult
		if err := r.p.Call(ctx, "validate", protocol.ValidateParams{Kind: protocol.ValidateExecutor, Type: name, Config: config}, &out); err != nil {
			return nil
		}
		var warnings []string
		for i, w := range out.Warnings {
			if i == maxWarnings {
				break
			}
			if w = oneLine(w); w != "" {
				warnings = append(warnings, w)
			}
		}
		return warnings
	}
}

// maxWarnings and maxWarning cap what a package's warnings put in front of
// the operator.
const maxWarnings, maxWarning = 8, 300

// oneLine is w on one line, capped at maxWarning characters.
func oneLine(w string) string {
	w = strings.Join(strings.Fields(w), " ")
	if r := []rune(w); len(r) > maxWarning {
		w = string(r[:maxWarning-1]) + "…"
	}
	return w
}

func (r *Remote) mode(p protocol.ValidateParams) string {
	ctx, cancel := context.WithTimeout(context.Background(), ValidateTimeout)
	defer cancel()
	var out protocol.ValidateResult
	if err := r.p.Call(ctx, "validate", p, &out); err != nil {
		return ""
	}
	return out.Mode
}

// ResourceSandbox returns the sandbox check of a resource type declared
// with sandbox: validate with kind resource_sandbox. A refusal's message is
// the package's reason, capped, which the sandbox_workspace diagnostic
// shows; a failed call is a refusal too, so an unreachable package never
// passes a sandboxed workspace it was asked to check.
func (r *Remote) ResourceSandbox(typ string) func(json.RawMessage, string) error {
	return func(config json.RawMessage, mode string) error {
		ctx, cancel := context.WithTimeout(context.Background(), ValidateTimeout)
		defer cancel()
		err := r.p.Call(ctx, "validate", protocol.ValidateParams{Kind: protocol.ValidateResourceSandbox, Type: typ, Config: config, Mode: mode}, nil)
		if err == nil {
			return nil
		}
		if msg, ok := refusal(err); ok {
			return errors.New(msg)
		}
		return errors.New("the resource provider could not check this workspace for a sandbox")
	}
}

// refusal is a validate refusal's reason, capped, when the package gave one.
func refusal(err error) (string, bool) {
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code != protocol.CodeInvalid || pe.Message == "" {
		return "", false
	}
	msg := pe.Message
	if len(msg) > maxBindingReason {
		msg = msg[:maxBindingReason]
	}
	return msg, true
}

// maxBindingReason caps a binding or sandbox refusal's reason.
const maxBindingReason = 300

// Webhook returns the http.Handler of a webhook path the extension
// registered at configure. The request crosses as webhook.handle; the
// extension verifies it and pushes through ingress.deliver before it
// answers, so a 2xx means durable acceptance. A failed call is 503, so the
// service retries (reconcile is the backstop).
func (r *Remote) Webhook(path string, maxBody int64) http.Handler {
	if maxBody <= 0 {
		maxBody = 1 << 20
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(io.LimitReader(req.Body, maxBody+1))
		if err != nil || int64(len(body)) > maxBody {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		var out protocol.WebhookHandleResult
		p := protocol.WebhookHandleParams{Path: path, Method: req.Method, Query: req.URL.RawQuery, Header: req.Header, Body: body, ReceivedAt: time.Now().UTC()}
		if err := r.p.Call(req.Context(), "webhook.handle", p, &out); err != nil {
			http.Error(w, "extension unavailable", http.StatusServiceUnavailable)
			return
		}
		for k, vs := range out.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if out.Status < 100 || out.Status > 599 {
			out.Status = http.StatusBadGateway
		}
		w.WriteHeader(out.Status)
		_, _ = io.Copy(w, bytes.NewReader(out.Body))
	})
}

// Provider returns the resource provider adapter of a declared type.
// Acquire is idempotent per owner and mode on the extension's side, so a
// crash mid-call is retried by the caller; Reconcile repairs the rest.
func (r *Remote) Provider(typ string) (extension.ResourceProvider, bool) {
	for _, d := range r.m.Provides.Resources {
		if d.Type == typ {
			return &remoteProvider{r: r, typ: typ}, true
		}
	}
	return nil, false
}

type remoteProvider struct {
	r   *Remote
	typ string
}

func (p *remoteProvider) Acquire(ctx context.Context, a extension.ResourceAcquisition) (extension.Resource, error) {
	var out protocol.ResourceResult
	err := p.r.p.Call(ctx, "provider.acquire", protocol.ProviderAcquireParams{Type: p.typ, Owner: protocol.FromOwner(a.Owner), Config: protocol.FromPayload(a.Config), Mode: a.Request.Mode, Params: protocol.FromPayload(a.Request.Params)}, &out)
	if a.Request.Log != nil && out.Log != "" {
		_, _ = io.WriteString(a.Request.Log, out.Log)
	}
	if err != nil {
		return extension.Resource{}, err
	}
	if out.SetupFailed {
		return out.Resource.Extension(), extension.ErrResourceSetup
	}
	return out.Resource.Extension(), nil
}

func (p *remoteProvider) Lookup(ctx context.Context, owner extension.ResourceOwner, ref string) (extension.Resource, error) {
	var out protocol.ResourceResult
	if err := p.r.p.Call(ctx, "provider.lookup", protocol.ProviderLookupParams{Type: p.typ, Owner: protocol.FromOwner(owner), Ref: ref}, &out); err != nil {
		return extension.Resource{}, err
	}
	return out.Resource.Extension(), nil
}

// Reconcile binds status to a fresh reconcile ID for the duration of the
// call: provider.owner_status quoting that ID is answered from the host's
// durable state; any other ID is refused.
func (p *remoteProvider) Reconcile(ctx context.Context, status extension.ResourceStatusFunc) (extension.ResourceReport, error) {
	id := newID()
	p.r.s.mu.Lock()
	p.r.s.reconciles[id] = status
	p.r.s.mu.Unlock()
	defer func() {
		p.r.s.mu.Lock()
		delete(p.r.s.reconciles, id)
		p.r.s.mu.Unlock()
	}()
	var out protocol.ResourceReport
	if err := p.r.p.Call(ctx, "provider.reconcile", protocol.ProviderReconcileParams{Type: p.typ, Reconcile: id}, &out); err != nil {
		return extension.ResourceReport{}, err
	}
	return out.Extension(), nil
}

// Exporter returns the telemetry exporter adapter (manifest exporters). It
// runs on the host pipeline's goroutine: a slow or failed batch is reported
// to the pipeline, which drops with accounting exactly as for an in-process
// exporter. Shutdown is the process's own shutdown.
func (r *Remote) Exporter() (observability.Exporter, bool) {
	if !r.m.Provides.Exporters {
		return nil, false
	}
	return remoteExporter{r}, true
}

type remoteExporter struct{ r *Remote }

func (e remoteExporter) Export(ctx context.Context, records []observability.Record) error {
	return e.r.p.Call(ctx, "telemetry.export", protocol.TelemetryExportParams{Records: protocol.FromRecords(records)}, nil)
}
func (e remoteExporter) Shutdown(context.Context) error { return nil }

// Sinks reports whether the package receives the host's telemetry events
// (permission telemetry.sink).
func (r *Remote) Sinks() bool { return r.m.Allows(protocol.PermissionTelemetrySink) }

// Capture hands the package one of the host's telemetry events
// (telemetry.sink), bounded; a package without the permission gets
// nothing.
func (r *Remote) Capture(ctx context.Context, scope, name string, at time.Time, props map[string]any) error {
	if !r.Sinks() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return r.p.Call(ctx, "telemetry.sink", protocol.TelemetryEvent{ScopeID: scope, Name: name, At: at, Properties: props}, nil)
}

// Annotate returns the extension's annotations for a Work (manifest
// annotate), bounded; a failure contributes nothing.
func (r *Remote) Annotate(ctx context.Context, w domain.Work) []protocol.Annotation {
	if !r.m.Provides.Annotate {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Every annotating extension sees every Work; what a source said about
	// a Work goes only to extensions configured for its scope.
	if r.s == nil || r.s.deps.Scopes == nil || !r.s.allowedScopes()[w.ScopeID] {
		w.Display = nil
	}
	var out protocol.WorkAnnotateResult
	if err := r.p.Call(ctx, "work.annotate", protocol.WorkAnnotateParams{Work: protocol.FromWork(w)}, &out); err != nil {
		return nil
	}
	return out.Annotations
}

// RunEnvironment asks for the extension's environment layer for a run
// (manifest run_environment); an extension that doesn't declare one has
// none. A failed call fails the run, as the contract says.
func (r *Remote) RunEnvironment(ctx context.Context, req extension.RunEnvironmentRequest) (map[string]string, error) {
	if !r.m.Provides.RunEnvironment {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var out protocol.RunEnvironmentResult
	if err := r.p.Call(ctx, "run.environment", protocol.RunEnvironmentParams{Owner: protocol.FromOwner(req.Owner), Connection: req.Connection}, &out); err != nil {
		return nil, err
	}
	return out.Env, nil
}

// Actions returns the extension's action provider (manifest actions), which
// is also its panel provider (manifest panels). A listing failure offers
// nothing; an invocation's plan is checked before the host accepts it: a
// family the extension declared, a payload it may write and, when keyed, a
// correlation key in its own source namespace.
func (r *Remote) Actions() (extension.ActionProvider, bool) {
	if !r.m.Provides.Actions && !r.m.Provides.Panels {
		return nil, false
	}
	return remoteActions{r}, true
}

type remoteActions struct{ r *Remote }

var _ extension.PanelProvider = remoteActions{}

// Panels returns the extension's panels on a target (view.render).
func (a remoteActions) Panels(ctx context.Context, t extension.ActionTarget) ([]extension.Panel, error) {
	if !a.r.m.Provides.Panels {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out protocol.ViewRenderResult
	if err := a.r.p.Call(ctx, "view.render", protocol.ViewRenderParams{Target: protocol.FromActionTarget(t)}, &out); err != nil {
		return nil, err
	}
	return protocol.ToPanels(out.Panels), nil
}

func (a remoteActions) Actions(ctx context.Context, t extension.ActionTarget) ([]extension.Action, error) {
	if !a.r.m.Provides.Actions {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out protocol.WorkActionsResult
	if err := a.r.p.Call(ctx, "work.actions", protocol.WorkActionsParams{Target: protocol.FromActionTarget(t)}, &out); err != nil {
		return nil, err
	}
	actions := make([]extension.Action, 0, len(out.Actions))
	for _, x := range out.Actions {
		actions = append(actions, extension.Action{ID: x.ID, Label: x.Label, Prompt: x.Prompt, Choices: x.Choices})
	}
	return actions, nil
}

func (a remoteActions) Invoke(ctx context.Context, c extension.ActionCall) (extension.ActionPlan, error) {
	if !a.r.m.Provides.Actions {
		return extension.ActionPlan{}, &extension.OperationError{Code: "unknown_action", Message: a.r.m.Name + " offers no actions"}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var out protocol.WorkActResult
	if err := a.r.p.Call(ctx, "work.act", protocol.WorkActParams{Target: protocol.FromActionTarget(c.Target), Action: c.Action, Input: c.Input, Caller: protocol.Caller{Kind: c.Caller.Kind, ID: c.Caller.ID}}, &out); err != nil {
		return extension.ActionPlan{}, err
	}
	e := out.Event.Domain()
	if !a.r.s.ownFamily(e.Type) {
		return extension.ActionPlan{}, protocol.Errorf(protocol.CodeUndeclared, "family not declared")
	}
	if e.CorrelationKey != nil && !protocol.SourceNamespace(a.r.m.Name, e.CorrelationKey.Source) {
		return extension.ActionPlan{}, protocol.Errorf(protocol.CodeUnauthorized, "correlation key is outside the extension's namespace")
	}
	if err := a.r.m.CheckWrite(e.Data); err != nil {
		return extension.ActionPlan{}, schemaError(err)
	}
	return extension.ActionPlan{Event: domain.Event{Type: e.Type, CorrelationKey: e.CorrelationKey, Data: e.Data}, Message: out.Message}, nil
}

// Setup returns the extension's `open-conduit init <type>` flow. Its
// setup.* callbacks are answered by the SetupHost of the run in progress
// and refused for any other run ID.
func (r *Remote) Setup() (extension.Setup, bool) {
	d := r.m.Provides.Setup
	if d == nil {
		return nil, false
	}
	return remoteSetup{r: r, info: extension.SetupInfo{Type: d.Type, Summary: d.Summary, Usage: d.Usage, FirstRun: d.FirstRun}}, true
}

type remoteSetup struct {
	r    *Remote
	info extension.SetupInfo
}

func (s remoteSetup) SetupInfo() extension.SetupInfo { return s.info }

func (s remoteSetup) Setup(ctx context.Context, host extension.SetupHost, args []string) (extension.SetupResult, error) {
	id := newID()
	svc := s.r.s
	svc.mu.Lock()
	svc.setups[id] = host
	svc.mu.Unlock()
	defer func() {
		svc.mu.Lock()
		delete(svc.setups, id)
		svc.mu.Unlock()
	}()
	var out protocol.SetupRunResult
	p := protocol.SetupRunParams{Run: id, Type: s.info.Type, Args: args, Config: host.Config(), Cwd: host.Cwd(), StateDir: host.StateDir(), DryRun: host.DryRun()}
	if err := s.r.p.Call(ctx, "setup.run", p, &out); err != nil {
		if errors.Is(err, extension.ErrInvalid) {
			return extension.SetupResult{}, errors.Join(extension.ErrSetupUsage, err)
		}
		return extension.SetupResult{}, err
	}
	return extension.SetupResult{Connections: out.Connections, Scope: out.Scope, Summary: out.Summary, Next: out.Next}, nil
}

// setup serves one setup.* callback through the run's SetupHost.
func (s *Service) setup(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var run struct {
		Run string `json:"run"`
	}
	if err := jsonrpc.Params(raw, &run); err != nil {
		return nil, err
	}
	s.mu.Lock()
	host := s.setups[run.Run]
	s.mu.Unlock()
	if host == nil {
		return nil, protocol.Errorf(protocol.CodeConflict, "no setup run in progress under that ID")
	}
	switch method {
	case "setup.print":
		var p protocol.SetupPrintParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		host.Printf("%s", p.Text)
		return struct{}{}, nil
	case "setup.prompt":
		var p protocol.SetupPromptParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		v, err := host.Prompt(p.Question, extension.Prompt{Default: p.Default, Secret: p.Secret})
		if errors.Is(err, extension.ErrSetupNoInput) {
			return nil, protocol.Errorf(protocol.CodeUnsupported, "no interactive input")
		}
		if err != nil {
			return nil, err
		}
		return protocol.SetupAnswer{Value: v}, nil
	case "setup.choose":
		var p protocol.SetupChooseParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		i, err := host.Choose(p.Question, p.Options)
		if errors.Is(err, extension.ErrSetupNoInput) {
			return nil, protocol.Errorf(protocol.CodeUnsupported, "no interactive input")
		}
		if err != nil {
			return nil, err
		}
		return protocol.SetupAnswer{Index: i}, nil
	case "setup.open_url":
		var p protocol.SetupURLParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return struct{}{}, host.OpenURL(p.URL)
	case "setup.store_secret":
		var p protocol.SetupSecretParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		ref, err := host.StoreSecret(p.Name, p.Value)
		if err != nil {
			return nil, err
		}
		return protocol.SetupAnswer{Value: ref}, nil
	case "setup.look_path":
		var p protocol.SetupLookPathParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		path, ok := host.LookPath(p.Name)
		return protocol.SetupAnswer{Value: path, Found: ok}, nil
	default: // setup.run_command
		var p protocol.SetupCommandParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return struct{}{}, host.Run(ctx, p.Dir, p.Name, p.Args...)
	}
}
