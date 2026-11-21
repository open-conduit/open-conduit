package extproc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
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
		var out protocol.ValidateResult
		if err := r.p.Call(ctx, "validate", protocol.ValidateParams{Kind: kind, Type: typ, Config: config, SecretNames: secretNames}, &out); err != nil {
			// The package's own diagnostics (extension.ConfigError) pass
			// through; any other refusal stays unexplained.
			if ds := extension.ConfigDiagnostics(err); len(ds) > 0 {
				return errors.Join(ErrInvalidConfig, &extension.ConfigError{Diagnostics: ds})
			}
			return ErrInvalidConfig
		}
		if kind == protocol.ValidateExtension || kind == protocol.ValidateRoute {
			r.described.put(describeKey(kind, typ, config), validated{out.Describe, out.WorkKinds, out.Bindings, out.Operations})
		}
		return nil
	}
}

// Described is what the package said a config object of an extension or
// route type sets up when it last validated that object
// (protocol.ValidateResult.Describe); ok is false when it has not
// validated it.
func (r *Remote) Described(kind, typ string, config json.RawMessage) (lines []protocol.DescribeLine, ok bool) {
	v, ok := r.described.get(describeKey(kind, typ, config))
	return v.lines, ok
}

// WorkKinds are the kinds of child Work the package said a route config of
// handler typ binds when it last validated that config
// (protocol.ValidateResult.WorkKinds); ok is false when it has not
// validated it.
func (r *Remote) WorkKinds(typ string, config json.RawMessage) (kinds []protocol.WorkKind, ok bool) {
	v, ok := r.described.get(describeKey(protocol.ValidateRoute, typ, config))
	return v.kinds, ok
}

// Bindings are the binding names the package said an entry config of
// extension type typ refers to when it last validated that config
// (protocol.ValidateResult.Bindings); ok is false when it has not
// validated it.
func (r *Remote) Bindings(typ string, config json.RawMessage) (names []string, ok bool) {
	v, ok := r.described.get(describeKey(protocol.ValidateExtension, typ, config))
	return v.bindings, ok
}

// EntryOperations are the operations the package said an entry config of
// extension type typ declares when it last validated that config
// (protocol.ValidateResult.Operations); ok is false when it has not
// validated it.
func (r *Remote) EntryOperations(typ string, config json.RawMessage) (ops []extension.EntryOperationDecl, ok bool) {
	v, ok := r.described.get(describeKey(protocol.ValidateExtension, typ, config))
	return v.operations, ok
}

// EntryHandler serves the package's entry operations through
// operation.call, with the entry each is called through in the caller's
// scope (protocol.OperationParams.Entry). entry finds that entry's type
// and config by its name; a call through an entry the scope doesn't have
// is refused as undeclared without reaching the package.
func (r *Remote) EntryHandler(entry func(scope domain.ID, name string) (typ string, config json.RawMessage, ok bool)) extension.EntryHandler {
	return func(ctx context.Context, a extension.Authority, c extension.EntryCall) (domain.Payload, error) {
		typ, config, ok := entry(a.ScopeID, c.Entry)
		if !ok {
			return domain.Payload{}, extension.ErrUndeclared
		}
		var out protocol.OperationResult
		params := protocol.OperationParams{Operation: extension.EntryOperationName(r.m.Name, c.Entry, c.Operation), Authority: protocol.FromAuthority(a), Input: protocol.FromPayload(c.Input), RequestID: extension.CallRequestID(ctx),
			Entry: &protocol.OperationEntry{Type: typ, Name: c.Entry, Operation: c.Operation, Config: config}}
		if err := r.p.Call(ctx, "operation.call", params, &out); err != nil {
			return domain.Payload{}, err
		}
		return out.Output.Domain(), nil
	}
}

// validated is what the package said about a config object it validated.
type validated struct {
	lines      []protocol.DescribeLine
	kinds      []protocol.WorkKind
	bindings   []string
	operations []extension.EntryOperationDecl
}

// describedCap bounds each of describedCache's two maps.
const describedCap = 512

// describedCache keeps what the package said (describe lines, a route's
// work kinds) about the objects validated lately,
// bounded however many drafts are validated (a setup session's or the
// config tools' candidates): once the newer map holds describedCap
// objects it becomes the older one and the oldest are dropped. Every
// validation of the configuration writes its objects' lines again, so the
// current generation's stay.
type describedCache struct {
	mu           sync.Mutex
	newer, older map[string]validated
}

func (c *describedCache) put(key string, lines validated) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.newer == nil {
		c.newer = map[string]validated{}
	}
	if _, ok := c.newer[key]; !ok && len(c.newer) >= describedCap {
		c.older, c.newer = c.newer, map[string]validated{}
	}
	c.newer[key] = lines
}

func (c *describedCache) get(key string) (validated, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if lines, ok := c.newer[key]; ok {
		return lines, true
	}
	lines, ok := c.older[key]
	return lines, ok
}

func describeKey(kind, typ string, config json.RawMessage) string {
	sum := sha256.Sum256(config)
	return kind + "/" + typ + "/" + hex.EncodeToString(sum[:])
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

// ExecutorPlaybook returns the playbook a declared executor starts its
// turns on with a binding config: validate with kind executor_playbook, ""
// for none or a failed call.
func (r *Remote) ExecutorPlaybook(name string) func(json.RawMessage) string {
	return func(config json.RawMessage) string {
		ctx, cancel := context.WithTimeout(context.Background(), ValidateTimeout)
		defer cancel()
		var out protocol.ValidateResult
		if err := r.p.Call(ctx, "validate", protocol.ValidateParams{Kind: protocol.ValidateExecutorPlaybook, Type: name, Config: config}, &out); err != nil {
			return ""
		}
		return oneLine(out.Playbook)
	}
}

// ExecutorVersionProbe returns the version probe of an executor declared
// with version_probe for a binding config: validate with kind
// executor_probe, no argv for none or a failed call.
func (r *Remote) ExecutorVersionProbe(name string) func(json.RawMessage) ([]string, []string) {
	return func(config json.RawMessage) ([]string, []string) {
		ctx, cancel := context.WithTimeout(context.Background(), ValidateTimeout)
		defer cancel()
		var out protocol.ValidateResult
		if err := r.p.Call(ctx, "validate", protocol.ValidateParams{Kind: protocol.ValidateExecutorProbe, Type: name, Config: config}, &out); err != nil || out.Probe == nil || len(out.Probe.Argv) == 0 {
			return nil, nil
		}
		return out.Probe.Argv, out.Probe.Env
	}
}

// ExecutorFolderConfig returns the folder hook of an executor declared with
// folder_config: validate with kind executor_folder, whose answer is the
// binding config adapted to the included folder it comes from. A refusal
// or a failed call is an error, so a binding is never stored unadapted.
func (r *Remote) ExecutorFolderConfig(name string) func(json.RawMessage, string, bool) (json.RawMessage, error) {
	return func(config json.RawMessage, folder string, resource bool) (json.RawMessage, error) {
		ctx, cancel := context.WithTimeout(context.Background(), ValidateTimeout)
		defer cancel()
		var out protocol.ValidateResult
		p := protocol.ValidateParams{Kind: protocol.ValidateExecutorFolder, Type: name, Config: config, Folder: folder, Resource: resource}
		if err := r.p.Call(ctx, "validate", p, &out); err != nil {
			return nil, errors.New("the extension could not adapt the binding config to its folder")
		}
		if !json.Valid(out.Config) {
			return nil, errors.New("the extension answered an adapted binding config that is not JSON")
		}
		return out.Config, nil
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

// sites reports whether the package declares typ with sites.
func (r *Remote) sites(typ string) bool {
	for _, d := range r.m.Provides.Resources {
		if d.Type == typ {
			return d.Sites
		}
	}
	return false
}

// ResourceSite returns the site check of a resource type declared with
// sites: validate with kind resource_site, whether a resource config can
// only be made at a site. A failed call says it can't be made anywhere
// else either, so a binding that doesn't place is refused rather than
// passed.
func (r *Remote) ResourceSite(typ string) func(json.RawMessage) bool {
	return func(config json.RawMessage) bool {
		ctx, cancel := context.WithTimeout(context.Background(), ValidateTimeout)
		defer cancel()
		var out protocol.ValidateResult
		if err := r.p.Call(ctx, "validate", protocol.ValidateParams{Kind: protocol.ValidateResourceSite, Type: typ, Config: config}, &out); err != nil {
			return true
		}
		return out.Site
	}
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
//
// The host reads at most the body limit the manifest declares
// (WebhookBodyLimit). A larger request is answered 413 with the limit,
// logged, and passed to refused when it is non-nil, without reaching the
// extension.
func (r *Remote) Webhook(path string, refused func(limit, length int64)) http.Handler {
	limit := r.WebhookBodyLimit()
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		tooLarge := func(length int64) {
			r.p.o.Log.Warn("webhook body over the extension's limit", "extension", r.m.Name, "path", path, "limit", limit, "length", length)
			if refused != nil {
				refused(limit, length)
			}
			http.Error(w, fmt.Sprintf("request body too large: the %s extension accepts at most %d bytes", r.m.Name, limit), http.StatusRequestEntityTooLarge)
		}
		if req.ContentLength > limit {
			tooLarge(req.ContentLength)
			return
		}
		body, err := io.ReadAll(io.LimitReader(req.Body, limit+1))
		if int64(len(body)) > limit {
			tooLarge(-1)
			return
		}
		if err != nil {
			http.Error(w, "request body unreadable", http.StatusBadRequest)
			return
		}
		var out protocol.WebhookHandleResult
		// The listener's receipt: when it, or the forwarder it
		// authenticated, received the delivery.
		rc, ok := extension.WebhookReceiptFrom(req.Context())
		if !ok {
			rc.ReceivedAt = time.Now()
		}
		p := protocol.WebhookHandleParams{Path: path, Method: req.Method, Query: req.URL.RawQuery, Header: req.Header, Body: body, ReceivedAt: rc.ReceivedAt.UTC(), ForwardID: rc.ForwardID}
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

// WebhookBodyLimit is the largest webhook request body the host reads for
// the extension: its manifest's webhook_max_bytes, or
// protocol.DefaultWebhookBodyBytes when it declares none. Validation keeps
// a declared limit within protocol.MaxWebhookBodyBytes.
func (r *Remote) WebhookBodyLimit() int64 {
	if n := r.m.Provides.WebhookMaxBytes; n > 0 {
		return min(n, protocol.MaxWebhookBodyBytes)
	}
	return protocol.DefaultWebhookBodyBytes
}

// Provider returns the resource provider adapter of a declared type.
// Acquire is idempotent per owner and mode on the extension's side, so a
// crash mid-call is retried by the caller; Reconcile repairs the rest.
func (r *Remote) Provider(typ string) (extension.ResourceProvider, bool) {
	for _, d := range r.m.Provides.Resources {
		if d.Type == typ {
			p := &remoteProvider{r: r, typ: typ}
			if d.Sweep {
				return sweepingProvider{p}, true
			}
			return p, true
		}
	}
	return nil, false
}

// sweepingProvider is a remote provider declared with sweep
// (extension.ResourceSweeper).
type sweepingProvider struct{ *remoteProvider }

// Sweep binds status to a fresh ID for the call, as Reconcile does, so the
// package's provider.owner_status quoting it is answered from the host's
// durable state.
func (p sweepingProvider) Sweep(ctx context.Context, status extension.ResourceStatusFunc) (extension.ResourceSweepReport, error) {
	id := p.r.ownerStatus(status)
	defer p.r.endOwnerStatus(id)
	var out protocol.ResourceSweepReport
	if err := p.r.p.Call(ctx, "provider.sweep", protocol.ProviderReconcileParams{Type: p.typ, Reconcile: id}, &out); err != nil {
		return extension.ResourceSweepReport{}, err
	}
	return out.Extension(), nil
}

// ownerStatus registers status under a fresh ID, which
// provider.owner_status quotes while the call that holds it runs.
func (r *Remote) ownerStatus(status extension.ResourceStatusFunc) string {
	id := newID()
	r.s.mu.Lock()
	r.s.reconciles[id] = status
	r.s.mu.Unlock()
	return id
}

func (r *Remote) endOwnerStatus(id string) {
	r.s.mu.Lock()
	delete(r.s.reconciles, id)
	r.s.mu.Unlock()
}

type remoteProvider struct {
	r   *Remote
	typ string
}

// Sandbox returns the process sandbox provider adapter of a declared type.
// The host binds it to an attempt (extension.BindSandbox), which merges the
// protected set into the needs and pins the plan's digest itself, so the
// package only turns a policy and needs into a wrapped launch.
func (r *Remote) Sandbox(typ string) (extension.SandboxProvider, bool) {
	for _, d := range r.m.Provides.Sandboxes {
		if d.Type == typ {
			return &remoteSandbox{r: r, typ: typ}, true
		}
	}
	return nil, false
}

type remoteSandbox struct {
	r   *Remote
	typ string
}

func (p *remoteSandbox) Plan(ctx context.Context, owner extension.ResourceOwner, policy extension.SandboxPolicy, needs extension.SandboxNeeds, launch extension.SandboxLaunch) (extension.SandboxPlan, error) {
	var out protocol.SandboxPlan
	err := p.r.p.Call(ctx, "sandbox_provider.plan", protocol.SandboxProviderPlanParams{Type: p.typ, Owner: protocol.FromOwner(owner), Config: protocol.FromPayload(policy.Config), Paths: protocol.FromSandboxPaths(policy.Paths), Needs: protocol.FromSandboxNeeds(needs), Launch: protocol.FromSandboxLaunch(launch)}, &out)
	if err != nil {
		return extension.SandboxPlan{}, sandboxErr(err)
	}
	return out.Extension(), nil
}

func (p *remoteSandbox) Adopt(ctx context.Context, plan extension.SandboxPlan, parent, root int) error {
	if err := p.r.p.Call(ctx, "sandbox_provider.adopt", protocol.SandboxProviderAdoptParams{Type: p.typ, Plan: protocol.FromSandboxPlan(plan), Parent: parent, Root: root}, nil); err != nil {
		return sandboxErr(err)
	}
	return nil
}

// Container returns the container provider adapter of a declared type.
// The host binds it to an attempt (extension.BindContainer), which merges
// the protected set into the needs, pins the plan's digest and the
// container, and keeps stop, reconcile and removal, so the package only
// plans, creates and reports containers. One declared with forward is an
// extension.ContainerForwarder, and one declared with place an
// extension.ContainerPlacer. Every one is an extension.ContainerCollector
// whose Collecting (extension.ContainerCollecting) is whether the type is
// declared with collect: only then does Collect call
// container_provider.collect.
func (r *Remote) Container(typ string) (extension.ContainerProvider, bool) {
	for _, d := range r.m.Provides.Containers {
		if d.Type == typ {
			p := &remoteContainer{r: r, typ: typ, collect: d.Collect}
			switch {
			case d.Forward && d.Place:
				return forwardingPlacingContainer{forwardingContainer{p}, placingContainer{p}}, true
			case d.Forward:
				return forwardingContainer{p}, true
			case d.Place:
				return placingContainer{p}, true
			}
			return p, true
		}
	}
	return nil, false
}

// Tunnel returns the tunnel provider adapter of a declared type. Each Run
// holds tunnel.run open until ctx ends, under a fresh run ID that the
// package's tunnel.url quotes; a URL quoting any other ID is refused, so a
// run the host has let go can't move its webhooks.
func (r *Remote) Tunnel(typ string) (extension.TunnelProvider, bool) {
	for _, d := range r.m.Provides.Tunnels {
		if d.Type == typ {
			return &remoteTunnel{r: r, typ: typ}, true
		}
	}
	return nil, false
}

// Verifier returns the credential verifier adapter of a declared type.
// Each Verify is one credential.verify call; any refusal, or the package
// being down, is an error the host turns into a 401.
func (r *Remote) Verifier(typ string) (extension.CredentialVerifier, bool) {
	for _, d := range r.m.Provides.Verifiers {
		if d.Type == typ {
			return &remoteVerifier{r: r, typ: typ}, true
		}
	}
	return nil, false
}

type remoteVerifier struct {
	r   *Remote
	typ string
}

func (v *remoteVerifier) Verify(ctx context.Context, c extension.CredentialCheck) (extension.VerifiedCredential, error) {
	var out protocol.CredentialVerifyResult
	if err := v.r.p.Call(ctx, "credential.verify", protocol.CredentialVerifyParams{Type: v.typ, Issuer: c.Issuer, Config: protocol.FromPayload(c.Config), Token: c.Token}, &out); err != nil {
		return extension.VerifiedCredential{}, err
	}
	at, err := time.Parse(time.RFC3339Nano, out.ExpiresAt)
	if err != nil {
		return extension.VerifiedCredential{}, fmt.Errorf("credential verifier %s answered without a valid expires_at", v.typ)
	}
	return extension.VerifiedCredential{Subject: out.Subject, Groups: out.Groups, ExpiresAt: at}, nil
}

// SecretProvider returns the secret provider adapter of a declared scheme.
// Each Serve is one secret.serve call: a refusal is an
// extension.SecretRefusal with the provider's code, and any other failure
// (the package being down, a malformed answer) is an error the host
// reports as provider_failed, failing closed.
func (r *Remote) SecretProvider(scheme string) (extension.SecretProvider, bool) {
	for _, d := range r.m.Provides.SecretSchemes {
		if d.Scheme == scheme {
			return &remoteSecrets{r: r}, true
		}
	}
	return nil, false
}

type remoteSecrets struct{ r *Remote }

func (v *remoteSecrets) Serve(ctx context.Context, req extension.SecretRequest) (string, error) {
	var out protocol.SecretServeResult
	if err := v.r.p.Call(ctx, "secret.serve", protocol.SecretServeParams{Scheme: req.Scheme, Reference: req.Reference}, &out); err != nil {
		return "", err
	}
	switch {
	case out.Code != "" && out.Value != "":
		return "", fmt.Errorf("secret provider %s answered with both a value and a code", req.Scheme)
	case out.Code != "":
		return "", &extension.SecretRefusal{Code: out.Code}
	}
	return out.Value, nil
}

type remoteContainer struct {
	r       *Remote
	typ     string
	collect bool
}

type remoteTunnel struct {
	r   *Remote
	typ string
}

// forwardingContainer is a remote container provider declared with forward.
type forwardingContainer struct{ *remoteContainer }

// placingContainer is a remote container provider declared with place.
type placingContainer struct{ *remoteContainer }

// forwardingPlacingContainer is one declared with both.
type forwardingPlacingContainer struct {
	forwardingContainer
	placing placingContainer
}

func (p forwardingPlacingContainer) Place(ctx context.Context, owner extension.ResourceOwner, policy extension.ContainerPolicy, prefer string) (extension.Placement, error) {
	return p.placing.Place(ctx, owner, policy, prefer)
}

func (p forwardingPlacingContainer) SiteRun(ctx context.Context, placement string, cmd extension.SiteCommand) (extension.SiteResult, error) {
	return p.placing.SiteRun(ctx, placement, cmd)
}

// Place fails closed: anything but the provider's own refusal means
// nothing may be acquired or run.
func (p placingContainer) Place(ctx context.Context, owner extension.ResourceOwner, policy extension.ContainerPolicy, prefer string) (extension.Placement, error) {
	var out protocol.Placement
	if err := p.r.p.Call(ctx, "container_provider.place", protocol.ContainerProviderPlaceParams{Type: p.typ, Owner: protocol.FromOwner(owner), Config: protocol.FromPayload(policy.Config), Prefer: prefer}, &out); err != nil {
		return extension.Placement{}, sandboxErr(err)
	}
	return out.Extension(), nil
}

// SiteRun's failure to reach the package is the site's: the command
// didn't run.
func (p placingContainer) SiteRun(ctx context.Context, placement string, cmd extension.SiteCommand) (extension.SiteResult, error) {
	var out protocol.SiteResult
	if err := p.r.p.Call(ctx, "container_provider.site_run", protocol.ContainerProviderSiteRunParams{Type: p.typ, Placement: placement, Command: protocol.FromSiteCommand(cmd)}, &out); err != nil {
		if errors.Is(err, extension.ErrSiteUnavailable) {
			return extension.SiteResult{}, err
		}
		return extension.SiteResult{}, errors.Join(extension.ErrSiteUnavailable, err)
	}
	return out.Extension(), nil
}

func (p forwardingContainer) Forward(ctx context.Context, config domain.Payload) (string, error) {
	var out protocol.ContainerForward
	if err := p.r.p.Call(ctx, "container_provider.forward", protocol.ContainerProviderForwardParams{Type: p.typ, Config: protocol.FromPayload(config)}, &out); err != nil {
		return "", sandboxErr(err)
	}
	return out.Address, nil
}

// Plan and Create fail closed as a sandbox does: a refusal stays itself
// and anything else means nothing may run.
func (p *remoteContainer) Plan(ctx context.Context, owner extension.ResourceOwner, policy extension.ContainerPolicy, needs extension.SandboxNeeds, launch extension.SandboxLaunch) (extension.ContainerPlan, error) {
	var out protocol.ContainerPlan
	err := p.r.p.Call(ctx, "container_provider.plan", protocol.ContainerProviderPlanParams{Type: p.typ, Owner: protocol.FromOwner(owner), Config: protocol.FromPayload(policy.Config), Paths: protocol.FromSandboxPaths(policy.Paths), Needs: protocol.FromSandboxNeeds(needs), Launch: protocol.FromSandboxLaunch(launch), Placement: policy.Placement}, &out)
	if err != nil {
		return extension.ContainerPlan{}, sandboxErr(err)
	}
	return out.Extension(), nil
}

func (p *remoteContainer) Create(ctx context.Context, plan extension.ContainerPlan, labels map[string]string) (extension.ContainerCreated, error) {
	var out protocol.ContainerCreated
	if err := p.r.p.Call(ctx, "container_provider.create", protocol.ContainerProviderCreateParams{Type: p.typ, Plan: protocol.FromContainerPlan(plan), Labels: labels}, &out); err != nil {
		return extension.ContainerCreated{}, sandboxErr(err)
	}
	return out.Extension(), nil
}

func (p *remoteContainer) Signal(ctx context.Context, ref extension.ContainerRef, signal string) error {
	return p.r.p.Call(ctx, "container_provider.signal", protocol.ContainerProviderRefParams{Type: p.typ, Ref: protocol.FromContainerRef(ref), Signal: signal}, nil)
}

func (p *remoteContainer) Stop(ctx context.Context, ref extension.ContainerRef, grace time.Duration) error {
	return p.r.p.Call(ctx, "container_provider.stop", protocol.ContainerProviderRefParams{Type: p.typ, Ref: protocol.FromContainerRef(ref), GraceMillis: grace.Milliseconds()}, nil)
}

// Inspect's failure is an error, never Gone: a package that is down can't
// confirm a container stopped.
func (p *remoteContainer) Inspect(ctx context.Context, ref extension.ContainerRef) (extension.ContainerState, error) {
	var out protocol.ContainerState
	if err := p.r.p.Call(ctx, "container_provider.inspect", protocol.ContainerProviderRefParams{Type: p.typ, Ref: protocol.FromContainerRef(ref)}, &out); err != nil {
		return extension.ContainerState{}, err
	}
	return out.Extension(), nil
}

// List keeps only what carries the owner's execution label.
func (p *remoteContainer) List(ctx context.Context, owner extension.ResourceOwner) ([]extension.ContainerState, error) {
	var out protocol.ContainerList
	if err := p.r.p.Call(ctx, "container_provider.list", protocol.ContainerProviderListParams{Type: p.typ, Owner: protocol.FromOwner(owner)}, &out); err != nil {
		return nil, err
	}
	// The host acts only on what carries the owner's labels, whatever the
	// package listed.
	states := make([]extension.ContainerState, 0, len(out.Containers))
	for _, st := range out.Containers {
		if owner.ExecutionID != "" && st.Ref.Labels[extension.ContainerLabelExecution] != string(owner.ExecutionID) {
			continue
		}
		states = append(states, st.Extension())
	}
	return states, nil
}

// Collecting is whether the type is declared with collect
// (extension.ContainerCollecting).
func (p *remoteContainer) Collecting() bool { return p.collect }

// Collect is nil for a type not declared with collect: its output came
// back with its wrapper, or never leaves the host.
func (p *remoteContainer) Collect(ctx context.Context, ref extension.ContainerRef) error {
	if !p.collect {
		return nil
	}
	return p.r.p.Call(ctx, "container_provider.collect", protocol.ContainerProviderRefParams{Type: p.typ, Ref: protocol.FromContainerRef(ref)}, nil)
}

func (p *remoteContainer) Remove(ctx context.Context, ref extension.ContainerRef) error {
	return p.r.p.Call(ctx, "container_provider.remove", protocol.ContainerProviderRefParams{Type: p.typ, Ref: protocol.FromContainerRef(ref)}, nil)
}

func (t *remoteTunnel) Run(ctx context.Context, run extension.TunnelRun, publish func(string)) error {
	id := newID()
	t.r.s.mu.Lock()
	t.r.s.tunnels[id] = publish
	t.r.s.mu.Unlock()
	defer func() {
		t.r.s.mu.Lock()
		delete(t.r.s.tunnels, id)
		t.r.s.mu.Unlock()
	}()
	err := t.r.p.Call(ctx, "tunnel.run", protocol.TunnelRunParams{Type: t.typ, Run: id, Addr: run.Addr, Config: protocol.FromPayload(run.Config)}, nil)
	switch {
	case ctx.Err() != nil:
		return nil
	case protocol.ErrorCode(err) == protocol.CodeInvalid:
		return errors.Join(extension.ErrTunnelConfig, err)
	case err == nil:
		// The package ended the run without being asked: the tunnel is
		// down, and the host starts it again.
		return errors.New("the tunnel provider ended its run")
	}
	return err
}

// sandboxErr fails a sandbox call closed: a refusal the package authored as
// sandbox_denied stays a denial, and anything else (the package down, an
// internal error) means the sandbox can't be enforced, so nothing runs.
func sandboxErr(err error) error {
	if errors.Is(err, extension.ErrSandboxDenied) || errors.Is(err, extension.ErrSandboxUnavailable) {
		return err
	}
	return errors.Join(extension.ErrSandboxUnavailable, err)
}

func (p *remoteProvider) Acquire(ctx context.Context, a extension.ResourceAcquisition) (extension.Resource, error) {
	var out protocol.ResourceResult
	params := protocol.ProviderAcquireParams{Type: p.typ, Owner: protocol.FromOwner(a.Owner), Config: protocol.FromPayload(a.Config), Mode: a.Request.Mode, Params: protocol.FromPayload(a.Request.Params), HostEnv: a.HostEnv, Files: a.Request.Files}
	// Only a provider declared with sites is given the site, under a call
	// ID site.run quotes while this acquisition runs.
	if a.Site != nil && p.r.sites(p.typ) {
		id := newID()
		p.r.s.mu.Lock()
		p.r.s.sites[id] = acquireSite{typ: p.typ, site: a.Site}
		p.r.s.mu.Unlock()
		defer func() {
			p.r.s.mu.Lock()
			delete(p.r.s.sites, id)
			p.r.s.mu.Unlock()
		}()
		params.Site = &protocol.AcquireSite{ID: a.Site.ID(), Call: id}
	}
	err := p.r.p.Call(ctx, "provider.acquire", params, &out)
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
	id := p.r.ownerStatus(status)
	defer p.r.endOwnerStatus(id)
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

// MetricExporter returns the metrics exporter adapter (permission
// telemetry.metrics):
// each collection of the host's meter goes to the package, bounded by the
// caller's context.
func (r *Remote) MetricExporter() (observability.MetricExporter, bool) {
	if !r.m.Allows(protocol.PermissionTelemetryMetrics) {
		return nil, false
	}
	return remoteMetrics{r}, true
}

type remoteMetrics struct{ r *Remote }

func (e remoteMetrics) ExportMetrics(ctx context.Context, metrics []observability.Metric) error {
	return e.r.p.Call(ctx, "telemetry.metrics", protocol.TelemetryMetricsParams{Metrics: metrics}, nil)
}

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
	layer, err := r.RunLayer(ctx, req)
	return layer.Env, err
}

// RunLayer is RunEnvironment with the files the layer carries, checked
// (extension.CheckRunFiles); a layer with files the host can't write
// fails the run as a failed call does. Files for a run with no FilesDir
// are dropped.
func (r *Remote) RunLayer(ctx context.Context, req extension.RunEnvironmentRequest) (extension.RunLayer, error) {
	if !r.m.Provides.RunEnvironment {
		return extension.RunLayer{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var out protocol.RunEnvironmentResult
	if err := r.p.Call(ctx, "run.environment", protocol.RunEnvironmentParams{Owner: protocol.FromOwner(req.Owner), Connection: req.Connection, Binding: req.Binding, FilesDir: req.FilesDir}, &out); err != nil {
		return extension.RunLayer{}, err
	}
	if req.FilesDir == "" {
		out.Files = nil
	}
	if err := extension.CheckRunFiles(out.Files); err != nil {
		return extension.RunLayer{}, err
	}
	return extension.RunLayer{Env: out.Env, Files: out.Files}, nil
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
		actions = append(actions, extension.Action{ID: x.ID, Label: x.Label, Prompt: x.Prompt, Choices: x.Choices, Detail: x.Detail, Awaited: x.Awaited})
	}
	return actions, nil
}

var _ extension.RequestProvider = remoteActions{}

// Requests returns the extension's requests on a target (work.requests).
// An extension that does not serve the method reports none.
func (a remoteActions) Requests(ctx context.Context, t extension.ActionTarget) ([]extension.Request, error) {
	if !a.r.m.Provides.Actions {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out protocol.WorkRequestsResult
	if err := a.r.p.Call(ctx, "work.requests", protocol.WorkRequestsParams{Target: protocol.FromActionTarget(t)}, &out); err != nil {
		var pe *protocol.Error
		if errors.As(err, &pe) && pe.Code == protocol.CodeMethodNotFound {
			return nil, nil
		}
		return nil, err
	}
	requests := make([]extension.Request, 0, len(out.Requests))
	for _, x := range out.Requests {
		requests = append(requests, x.Extension())
	}
	return requests, nil
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
