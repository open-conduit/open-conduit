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
		case protocol.ValidateResourceMode, protocol.ValidateResourceSandbox, protocol.ValidateExecutorMode:
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
		res, err := prov.Acquire(ctx, extension.ResourceAcquisition{Owner: p.Owner.Extension(), Config: p.Config.Domain(), Request: extension.ResourceRequest{Mode: p.Mode, Params: p.Params.Domain(), Log: &log}})
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
			out.Actions = append(out.Actions, protocol.Action{ID: a.ID, Label: a.Label, Prompt: a.Prompt, Choices: a.Choices})
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
// executor request (extension.ResourceModer). A type that doesn't implement
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
	}
	return protocol.ValidateResult{}, nil
}
