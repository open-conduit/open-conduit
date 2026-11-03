package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
	"github.com/open-conduit/open-conduit/storage"
)

// surface serves the host-to-extension methods 07-03 adapted.
func (s *Server) surface(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "validate":
		var p protocol.ValidateParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		switch p.Kind {
		case protocol.ValidateResourceMode, protocol.ValidateResourceSandbox, protocol.ValidateExecutorMode, protocol.ValidateExecutorPlaybook:
			return s.workspaceCheck(p)
		}
		if s.ext.Validate != nil {
			if err := s.ext.Validate(ctx, p); err != nil {
				if protocol.ErrorCode(err) != "" {
					return nil, err
				}
				if p.Kind == protocol.ValidateBinding {
					// The reason is what the operator reads.
					return nil, protocol.Errorf(protocol.CodeInvalid, err.Error())
				}
				return nil, protocol.Errorf(protocol.CodeInvalid, "invalid "+p.Kind+" config")
			}
		}
		if w, ok := s.ext.Executors[p.Type].(extension.ConfigWarner); ok && p.Kind == protocol.ValidateExecutor {
			return protocol.ValidateResult{Warnings: w.ConfigWarnings(p.Config)}, nil
		}
		if s.ext.Describe != nil && (p.Kind == protocol.ValidateExtension || p.Kind == protocol.ValidateRoute) {
			if lines := s.ext.Describe(p); len(lines) > 0 {
				return protocol.ValidateResult{Describe: lines}, nil
			}
		}
		return struct{}{}, nil
	case "webhook.handle":
		var p protocol.WebhookHandleParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if s.ext.Webhooks == nil {
			return nil, notFound(method)
		}
		h, ok := s.ext.Webhooks(p.Path)
		if !ok {
			return protocol.WebhookHandleResult{Status: http.StatusNotFound}, nil
		}
		return serveWebhook(ctx, h, p), nil
	case "provider.acquire":
		var p protocol.ProviderAcquireParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		prov, ok := s.ext.Providers[p.Type]
		if !ok {
			return nil, notFound(method)
		}
		var log bytes.Buffer
		res, err := prov.Acquire(ctx, extension.ResourceAcquisition{Owner: p.Owner.Extension(), Config: p.Config.Domain(), Request: extension.ResourceRequest{Mode: p.Mode, Params: p.Params.Domain(), Log: &log}, HostEnv: p.HostEnv})
		out := protocol.ResourceResult{Resource: protocol.FromResource(res), Log: log.String()}
		if err != nil {
			if !isSetup(err) {
				return nil, err
			}
			out.SetupFailed = true
		}
		return out, nil
	case "provider.lookup":
		var p protocol.ProviderLookupParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		prov, ok := s.ext.Providers[p.Type]
		if !ok {
			return nil, notFound(method)
		}
		res, err := prov.Lookup(ctx, p.Owner.Extension(), p.Ref)
		if err != nil {
			return nil, err
		}
		return protocol.ResourceResult{Resource: protocol.FromResource(res)}, nil
	case "provider.reconcile":
		var p protocol.ProviderReconcileParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		prov, ok := s.ext.Providers[p.Type]
		if !ok {
			return nil, notFound(method)
		}
		status := func(ctx context.Context, o extension.ResourceOwner) (extension.ResourceOwnerStatus, error) {
			var out protocol.OwnerStatus
			err := s.conn.Call(ctx, "provider.owner_status", protocol.OwnerStatusParams{Reconcile: p.Reconcile, Owner: protocol.FromOwner(o)}, &out)
			return out.Extension(), err
		}
		rep, err := prov.Reconcile(ctx, status)
		if err != nil {
			return nil, err
		}
		return protocol.FromReport(rep), nil
	case "provider.sweep":
		var p protocol.ProviderReconcileParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		prov, ok := s.ext.Providers[p.Type].(extension.ResourceSweeper)
		if !ok {
			return nil, notFound(method)
		}
		status := func(ctx context.Context, o extension.ResourceOwner) (extension.ResourceOwnerStatus, error) {
			var out protocol.OwnerStatus
			err := s.conn.Call(ctx, "provider.owner_status", protocol.OwnerStatusParams{Reconcile: p.Reconcile, Owner: protocol.FromOwner(o)}, &out)
			return out.Extension(), err
		}
		rep, err := prov.Sweep(ctx, status)
		if err != nil {
			return nil, err
		}
		return protocol.FromSweepReport(rep), nil
	case "sandbox_provider.plan":
		var p protocol.SandboxProviderPlanParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		prov, ok := s.ext.Sandboxes[p.Type]
		if !ok {
			return nil, notFound(method)
		}
		plan, err := prov.Plan(ctx, p.Owner.Extension(), extension.SandboxPolicy{Config: p.Config.Domain(), Paths: p.Paths.Extension()}, p.Needs.Extension(), p.Launch.Extension())
		if err != nil {
			return nil, err
		}
		return protocol.FromSandboxPlan(plan), nil
	case "sandbox_provider.adopt":
		var p protocol.SandboxProviderAdoptParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		prov, ok := s.ext.Sandboxes[p.Type]
		if !ok {
			return nil, notFound(method)
		}
		if err := prov.Adopt(ctx, p.Plan.Extension(), p.Parent, p.Root); err != nil {
			return nil, err
		}
		return struct{}{}, nil
	case "container_provider.plan":
		var p protocol.ContainerProviderPlanParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		prov, ok := s.ext.Containers[p.Type]
		if !ok {
			return nil, notFound(method)
		}
		plan, err := prov.Plan(ctx, p.Owner.Extension(), extension.ContainerPolicy{Config: p.Config.Domain(), Paths: p.Paths.Extension()}, p.Needs.Extension(), p.Launch.Extension())
		if err != nil {
			return nil, err
		}
		return protocol.FromContainerPlan(plan), nil
	case "container_provider.create":
		var p protocol.ContainerProviderCreateParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		prov, ok := s.ext.Containers[p.Type]
		if !ok {
			return nil, notFound(method)
		}
		c, err := prov.Create(ctx, p.Plan.Extension(), p.Labels)
		if err != nil {
			return nil, err
		}
		return protocol.FromContainerCreated(c), nil
	case "container_provider.signal", "container_provider.stop", "container_provider.inspect", "container_provider.remove":
		var p protocol.ContainerProviderRefParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		prov, ok := s.ext.Containers[p.Type]
		if !ok {
			return nil, notFound(method)
		}
		ref := p.Ref.Extension()
		var err error
		switch method {
		case "container_provider.signal":
			err = prov.Signal(ctx, ref, p.Signal)
		case "container_provider.stop":
			err = prov.Stop(ctx, ref, time.Duration(p.GraceMillis)*time.Millisecond)
		case "container_provider.remove":
			err = prov.Remove(ctx, ref)
		default:
			st, err := prov.Inspect(ctx, ref)
			if err != nil {
				return nil, err
			}
			return protocol.FromContainerState(st), nil
		}
		if err != nil {
			return nil, err
		}
		return struct{}{}, nil
	case "container_provider.list":
		var p protocol.ContainerProviderListParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		prov, ok := s.ext.Containers[p.Type]
		if !ok {
			return nil, notFound(method)
		}
		states, err := prov.List(ctx, p.Owner.Extension())
		if err != nil {
			return nil, err
		}
		out := protocol.ContainerList{Containers: []protocol.ContainerState{}}
		for _, st := range states {
			out.Containers = append(out.Containers, protocol.FromContainerState(st))
		}
		return out, nil
	case "container_provider.forward":
		var p protocol.ContainerProviderForwardParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		prov, ok := s.ext.Containers[p.Type].(extension.ContainerForwarder)
		if !ok {
			return nil, notFound(method)
		}
		addr, err := prov.Forward(ctx, p.Config.Domain())
		if err != nil {
			return nil, err
		}
		return protocol.ContainerForward{Address: addr}, nil
	case "telemetry.export":
		var p protocol.TelemetryExportParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if s.ext.Exporter == nil {
			return nil, notFound(method)
		}
		return struct{}{}, s.ext.Exporter.Export(ctx, protocol.ToRecords(p.Records))
	case "run.environment":
		var p protocol.RunEnvironmentParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if s.ext.RunEnvironment == nil {
			return nil, notFound(method)
		}
		env, err := s.ext.RunEnvironment(ctx, s.host, extension.RunEnvironmentRequest{Owner: p.Owner.Extension(), Connection: p.Connection})
		if err != nil {
			return nil, err
		}
		return protocol.RunEnvironmentResult{Env: env}, nil
	case "work.annotate":
		var p protocol.WorkAnnotateParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if s.ext.Annotate == nil {
			return nil, notFound(method)
		}
		lines, err := s.ext.Annotate(ctx, s.host, p.Work.Domain())
		if err != nil {
			return nil, err
		}
		return protocol.WorkAnnotateResult{Annotations: lines}, nil
	case "work.actions":
		var p protocol.WorkActionsParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if s.ext.Actions == nil {
			return nil, notFound(method)
		}
		actions, err := s.ext.Actions(ctx, s.host, p.Target.Extension())
		if err != nil {
			return nil, err
		}
		out := protocol.WorkActionsResult{}
		for _, a := range actions {
			out.Actions = append(out.Actions, protocol.Action{ID: a.ID, Label: a.Label, Prompt: a.Prompt, Choices: a.Choices, Detail: a.Detail})
		}
		return out, nil
	case "view.render":
		var p protocol.ViewRenderParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if s.ext.Panels == nil {
			return nil, notFound(method)
		}
		panels, err := s.ext.Panels(ctx, s.host, p.Target.Extension())
		if err != nil {
			return nil, err
		}
		return protocol.ViewRenderResult{Panels: protocol.FromPanels(panels)}, nil
	case "config.candidate":
		var p protocol.ConfigCandidateParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if s.ext.ConfigCandidate == nil {
			return nil, notFound(method)
		}
		c, err := s.ext.ConfigCandidate(ctx, s.host, p.Work.Domain())
		if errors.Is(err, storage.ErrNotFound) {
			return protocol.ConfigCandidateResult{}, nil
		}
		if err != nil {
			return nil, err
		}
		return protocol.FromCandidate(c), nil
	case "work.act":
		var p protocol.WorkActParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if s.ext.Act == nil {
			return nil, notFound(method)
		}
		plan, err := s.ext.Act(ctx, s.host, extension.ActionCall{Target: p.Target.Extension(), Action: p.Action, Input: p.Input, Caller: extension.Caller{Kind: p.Caller.Kind, ID: p.Caller.ID}})
		if err != nil {
			return nil, err
		}
		return protocol.WorkActResult{Event: protocol.FromEvent(plan.Event), Message: plan.Message}, nil
	case "probe.serve":
		var p protocol.ProbeRunParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		for _, probe := range s.ext.Probes {
			if probe.Name != p.Name {
				continue
			}
			res, err := probe.Run(ctx, p.Args)
			if err != nil {
				// A probe that ran and failed answers its reason, which
				// must be fixed and secret-free (extension.SetupProbe).
				return protocol.ProbeRunResult{Error: err.Error()}, nil
			}
			out, err := json.Marshal(res.Output)
			if err != nil {
				return nil, err
			}
			return protocol.ProbeRunResult{Command: res.Command, Output: out}, nil
		}
		return nil, notFound(method)
	case "setup.run":
		var p protocol.SetupRunParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		setup, ok := s.ext.Setups[p.Type]
		if !ok {
			return nil, notFound(method)
		}
		res, err := setup.Setup(ctx, &setupHost{conn: s.conn, p: p, ctx: ctx}, p.Args)
		if err != nil {
			return nil, err
		}
		return protocol.SetupRunResult{Connections: res.Connections, Scope: res.Scope, Summary: res.Summary, Next: res.Next}, nil
	}
	return nil, notFound(method)
}

func isSetup(err error) bool { return errors.Is(err, extension.ErrResourceSetup) }

// recorder is the minimal http.ResponseWriter a webhook handler writes to.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

func serveWebhook(ctx context.Context, h http.Handler, p protocol.WebhookHandleParams) protocol.WebhookHandleResult {
	u := &url.URL{Path: p.Path, RawQuery: p.Query}
	req, err := http.NewRequestWithContext(ctx, p.Method, u.String(), io.NopCloser(bytes.NewReader(p.Body)))
	if err != nil {
		return protocol.WebhookHandleResult{Status: http.StatusBadRequest}
	}
	req.Header = http.Header(p.Header)
	if req.Header == nil {
		req.Header = http.Header{}
	}
	req.ContentLength = int64(len(p.Body))
	rec := &recorder{header: http.Header{}}
	h.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return protocol.WebhookHandleResult{Status: rec.status, Header: rec.header, Body: rec.body.Bytes()}
}

// setupHost is extension.SetupHost over the setup.* callbacks of one run.
type setupHost struct {
	conn *jsonrpc.Conn
	p    protocol.SetupRunParams
	ctx  context.Context
}

func (h *setupHost) Printf(format string, args ...any) {
	_ = h.conn.Call(h.ctx, "setup.print", protocol.SetupPrintParams{Run: h.p.Run, Text: fmt.Sprintf(format, args...)}, nil)
}
func (h *setupHost) Prompt(question string, p extension.Prompt) (string, error) {
	var out protocol.SetupAnswer
	if err := h.conn.Call(h.ctx, "setup.prompt", protocol.SetupPromptParams{Run: h.p.Run, Question: question, Default: p.Default, Secret: p.Secret}, &out); err != nil {
		if protocol.ErrorCode(err) == protocol.CodeUnsupported {
			return "", extension.ErrSetupNoInput
		}
		return "", err
	}
	return out.Value, nil
}
func (h *setupHost) Choose(question string, options []string) (int, error) {
	var out protocol.SetupAnswer
	if err := h.conn.Call(h.ctx, "setup.choose", protocol.SetupChooseParams{Run: h.p.Run, Question: question, Options: options}, &out); err != nil {
		if protocol.ErrorCode(err) == protocol.CodeUnsupported {
			return 0, extension.ErrSetupNoInput
		}
		return 0, err
	}
	return out.Index, nil
}
func (h *setupHost) OpenURL(u string) error {
	return h.conn.Call(h.ctx, "setup.open_url", protocol.SetupURLParams{Run: h.p.Run, URL: u}, nil)
}
func (h *setupHost) StoreSecret(name, value string) (string, error) {
	var out protocol.SetupAnswer
	err := h.conn.Call(h.ctx, "setup.store_secret", protocol.SetupSecretParams{Run: h.p.Run, Name: name, Value: value}, &out)
	return out.Value, err
}
func (h *setupHost) LookPath(name string) (string, bool) {
	var out protocol.SetupAnswer
	if err := h.conn.Call(h.ctx, "setup.look_path", protocol.SetupLookPathParams{Run: h.p.Run, Name: name}, &out); err != nil {
		return "", false
	}
	return out.Value, out.Found
}
func (h *setupHost) Run(ctx context.Context, dir, name string, args ...string) error {
	return h.conn.Call(ctx, "setup.run_command", protocol.SetupCommandParams{Run: h.p.Run, Dir: dir, Name: name, Args: args}, nil)
}
func (h *setupHost) Config() []byte   { return h.p.Config }
func (h *setupHost) Cwd() string      { return h.p.Cwd }
func (h *setupHost) StateDir() string { return h.p.StateDir }
func (h *setupHost) DryRun() bool     { return h.p.DryRun }

var _ extension.SetupHost = (*setupHost)(nil)

// workspaceCheck answers the validate kinds a resource provider or executor
// answers through its optional interfaces: the mode a resource config names
// (extension.ResourceModeDefaulter), whether a sandboxed binding may use it
// (extension.ResourceSandboxChecker) and the mode a binding config makes an
// executor request (extension.ResourceModer), and the playbook a binding
// config starts its turns on (extension.PlaybookNamer). A type that doesn't implement
// the interface names no mode and passes the sandbox check, as a first-party
// registration without the hook does.
func (s *Server) workspaceCheck(p protocol.ValidateParams) (any, error) {
	switch p.Kind {
	case protocol.ValidateResourceMode:
		if d, ok := s.ext.Providers[p.Type].(extension.ResourceModeDefaulter); ok {
			return protocol.ValidateResult{Mode: d.ModeDefault(p.Config)}, nil
		}
	case protocol.ValidateResourceSandbox:
		if c, ok := s.ext.Providers[p.Type].(extension.ResourceSandboxChecker); ok {
			if err := c.SandboxWorkspace(p.Config, p.Mode); err != nil {
				// The reason is what the operator reads.
				return nil, protocol.Errorf(protocol.CodeInvalid, err.Error())
			}
		}
	case protocol.ValidateExecutorMode:
		if m, ok := s.ext.Executors[p.Type].(extension.ResourceModer); ok {
			return protocol.ValidateResult{Mode: m.ResourceMode(p.Config)}, nil
		}
	case protocol.ValidateExecutorPlaybook:
		if n, ok := s.ext.Executors[p.Type].(extension.PlaybookNamer); ok {
			return protocol.ValidateResult{Playbook: n.Playbook(p.Config)}, nil
		}
	}
	return protocol.ValidateResult{}, nil
}
