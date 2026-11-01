// Package sdktest is an in-memory host for unit-testing an extension's
// handlers. New serves an sdk.Extension over a real jsonrpc pipe, as the
// daemon would, and answers the scoped extension-to-host methods from
// memory: KV with versions and conflicts, event.accept and ingress.deliver,
// the state.* reads of seeded scopes, Works and Events, secrets from
// options, artifacts and telemetry.capture. A test then calls into the
// extension (Operation, Configure, Route, RunTask, Check, Call) and
// inspects what it wrote (KV, Events, Commits, Artifacts, Captured).
//
// It is not the host: causal depth and rate limits, timers, Work control,
// probes and config.* are not served (they answer CodeUnsupported), and a
// method the manifest does not enable is refused with CodeUndeclared, as the
// host refuses it. To see a package in the real daemon, use extensiontest.
package sdktest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// Clock is the host's clock. Pass conformance.NewClock() and advance it in
// the test; tests never wait on real time.
type Clock interface{ Now() time.Time }

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// Start is the default clock's fixed time (conformance.NewClock's start).
var Start = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// Option configures New.
type Option func(*Host)

// WithClock sets the host's clock (default: fixed at Start).
func WithClock(c Clock) Option { return func(h *Host) { h.clock = c } }

// WithScopes configures scopes for the extension: only these (and the
// scopes of Configure's entries) are visible to state reads, secrets and
// event.accept.
func WithScopes(scopes ...domain.Scope) Option {
	return func(h *Host) {
		for _, s := range scopes {
			h.addScope(s)
		}
	}
}

// WithWorks seeds Work. Its scope is configured.
func WithWorks(works ...domain.Work) Option {
	return func(h *Host) {
		for _, w := range works {
			h.addScope(domain.Scope{ID: w.ScopeID, Name: string(w.ScopeID)})
			h.works = append(h.works, w)
		}
	}
}

// WithExecutions seeds Executions, read with state.executions.
func WithExecutions(xs ...domain.Execution) Option {
	return func(h *Host) { h.executions = append(h.executions, xs...) }
}

// WithEvents seeds accepted Events in scope, in order, as ingress would
// accept them: each is a causal root. An Event without an ID gets one from
// its source and delivery ID; an unset ReceivedAt is the clock's now.
func WithEvents(scope domain.ID, events ...domain.Event) Option {
	return func(h *Host) {
		h.addScope(domain.Scope{ID: scope, Name: string(scope)})
		h.seeds = append(h.seeds, func() {
			for _, e := range events {
				h.acceptLocked(scope, e, "")
			}
		})
	}
}

// WithSecrets sets the secret values an entry of the extension in scope
// resolves (secret.resolve without a connection).
func WithSecrets(scope domain.ID, values map[string]string) Option {
	return func(h *Host) {
		h.addScope(domain.Scope{ID: scope, Name: string(scope)})
		h.secrets[secretKey{scope, ""}] = values
	}
}

// WithConnectionSecrets sets the secret values of a connection in scope
// (Host.ConnectionSecrets, or an operation acting as its binding's
// connection).
func WithConnectionSecrets(scope domain.ID, connection string, values map[string]string) Option {
	return func(h *Host) {
		h.addScope(domain.Scope{ID: scope, Name: string(scope)})
		h.secrets[secretKey{scope, connection}] = values
	}
}

// WithKV seeds a KV value.
func WithKV(namespace, key string, value []byte) Option {
	return func(h *Host) { h.kvPut(namespace, key, value) }
}

// WithKVVersion sets the KV layout the host has recorded for the
// extension, so Configure asks it to migrate from that version (default:
// the manifest's kv_version, so no migration runs).
func WithKVVersion(v int) Option { return func(h *Host) { h.kvVersion = v } }

// WithStateDir sets the private state directory given at initialize
// (default: t.TempDir()).
func WithStateDir(dir string) Option { return func(h *Host) { h.stateDir = dir } }

// WithAuthority sets the authority Operation calls carry. Unset fields
// default as Authority describes.
func WithAuthority(a extension.Authority) Option { return func(h *Host) { h.authority = a } }

// Host is the in-memory host serving one extension.
type Host struct {
	t        testing.TB
	ext      *sdk.Extension
	conn     *jsonrpc.Conn
	server   *sdk.Server
	clock    Clock
	stateDir string
	enabled  map[string]bool

	mu         sync.Mutex
	scopes     []domain.Scope
	works      []domain.Work
	executions []domain.Execution
	events     []domain.AcceptedEvent
	causal     map[domain.ID]causal
	kv         map[string]map[string][]byte
	kvVersion  int
	commits    []protocol.KVCommitParams
	secrets    map[secretKey]map[string]string
	sources    map[string]protocol.SourceDecl
	writers    map[string]*writer
	artifacts  []Artifact
	captured   []protocol.TelemetryEvent
	authority  extension.Authority
	calls      int
	seeds      []func() // run once every option is applied (the clock is set)
}

type secretKey struct {
	scope      domain.ID
	connection string
}

type causal struct {
	scope domain.ID
	root  domain.ID
	depth int
}

type writer struct {
	mediaType string
	data      []byte
}

// Artifact is one committed artifact.
type Artifact struct {
	Ref  domain.ArtifactRef
	Data []byte
}

// New serves ext on an in-memory host and initializes it. The connection
// closes when the test ends. It fails the test on an invalid manifest or a
// refused initialize.
func New(t testing.TB, ext *sdk.Extension, opts ...Option) *Host {
	t.Helper()
	if err := ext.Manifest.Validate(); err != nil {
		t.Fatalf("sdktest: manifest: %v", err)
	}
	h := &Host{t: t, ext: ext, clock: fixedClock{Start}, kvVersion: ext.Manifest.KVVersion,
		causal: map[domain.ID]causal{}, kv: map[string]map[string][]byte{}, secrets: map[secretKey]map[string]string{},
		sources: map[string]protocol.SourceDecl{}, writers: map[string]*writer{}, enabled: map[string]bool{}}
	for _, m := range ext.Manifest.Enabled() {
		h.enabled[m] = true
	}
	for _, o := range opts {
		o(h)
	}
	for _, seed := range h.seeds {
		seed()
	}
	if h.stateDir == "" {
		h.stateDir = t.TempDir()
	}
	sr, hw := io.Pipe()
	hr, sw := io.Pipe()
	h.server = sdk.NewServer(ext, sr, sw)
	h.conn = jsonrpc.NewConn(hr, hw, h.handle, jsonrpc.Options{})
	t.Cleanup(func() {
		_ = h.conn.Close()
		_ = hw.Close()
		_ = sw.Close()
		<-h.server.Done()
	})
	if err := h.conn.Call(context.Background(), "initialize", protocol.InitializeParams{ProtocolVersions: ext.Manifest.Protocol, StateDir: h.stateDir}, nil); err != nil {
		t.Fatalf("sdktest: initialize: %v", err)
	}
	return h
}

// Server is the served extension (its StateDir and Host).
func (h *Host) Server() *sdk.Server { return h.server }

// StateDir is the private state directory given at initialize.
func (h *Host) StateDir() string { return h.stateDir }

// Now is the host clock's time.
func (h *Host) Now() time.Time { return h.clock.Now() }

// Calls into the extension.

// Call makes any host-to-extension call, for a surface without a helper.
func (h *Host) Call(ctx context.Context, method string, params, out any) error {
	return h.conn.Call(ctx, method, params, out)
}

// Configure applies a configuration generation. With p.KV unset it sends
// the recorded KV version and the manifest's, so a WithKVVersion below the
// manifest's runs Migrate first. Each entry's scope becomes configured, and
// the sources the result registers can be delivered to.
func (h *Host) Configure(ctx context.Context, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
	if p.KV == (protocol.KVState{}) {
		h.mu.Lock()
		p.KV = protocol.KVState{Stored: h.kvVersion, Declared: h.ext.Manifest.KVVersion}
		h.mu.Unlock()
	}
	if p.Generation == "" {
		p.Generation = "sdktest"
	}
	var out protocol.ConfigureResult
	if err := h.conn.Call(ctx, "configure", p, &out); err != nil {
		return out, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range p.Entries {
		h.addScope(domain.Scope{ID: domain.ID(e.ScopeID), Name: e.ScopeID})
	}
	h.sources = map[string]protocol.SourceDecl{}
	for _, s := range out.Sources {
		h.sources[s.Name] = s
	}
	h.kvVersion = out.KVVersion
	return out, nil
}

// OperationResult is what an operation returned. Output is a handler's
// output, a wait plan's output or an emission's commit output. Wait is the
// plan of a wait operation; Event the Event an emission accepted.
type OperationResult struct {
	Output domain.Payload
	Wait   *extension.WaitPlan
	Event  *extension.Emitted
}

// Operation calls a declared toolbelt operation with the default authority
// (see OperationAs).
func (h *Host) Operation(ctx context.Context, name string, input domain.Payload) (OperationResult, error) {
	return h.OperationAs(ctx, h.defaultAuthority(), name, input)
}

func (h *Host) defaultAuthority() extension.Authority {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.authority
	if a.ScopeID == "" && len(h.scopes) > 0 {
		a.ScopeID = h.scopes[0].ID
	}
	return a
}

// OperationAs calls a declared toolbelt operation with authority a, by the
// kind its manifest declares. Unset fields of a default: WorkID
// "sdktest-work", ExecutionID "sdktest-execution", ExpiresAt an hour after
// now, Operations every declared operation and EventTypes every declared
// family. An emission is accepted in a.ScopeID with a.TriggerID as its
// cause and its commit's records written to KV, as the toolbelt would.
func (h *Host) OperationAs(ctx context.Context, a extension.Authority, name string, input domain.Payload) (OperationResult, error) {
	var decl *protocol.OperationDecl
	for i, d := range h.ext.Manifest.Provides.Operations {
		if d.Name == name {
			decl = &h.ext.Manifest.Provides.Operations[i]
		}
	}
	if decl == nil {
		return OperationResult{}, protocol.Errorf(protocol.CodeUndeclared, "operation "+name+" is not declared")
	}
	if a.WorkID == "" {
		a.WorkID = "sdktest-work"
	}
	if a.ExecutionID == "" {
		a.ExecutionID = "sdktest-execution"
	}
	if a.ExpiresAt.IsZero() {
		a.ExpiresAt = h.Now().Add(time.Hour)
	}
	if a.Operations == nil {
		for _, d := range h.ext.Manifest.Provides.Operations {
			a.Operations = append(a.Operations, d.Name)
		}
	}
	if a.EventTypes == nil {
		for _, f := range h.ext.Manifest.Provides.Families {
			a.EventTypes = append(a.EventTypes, f.Type)
		}
	}
	h.mu.Lock()
	h.calls++
	request := "sdktest-" + strconv.Itoa(h.calls)
	h.mu.Unlock()
	p := protocol.OperationParams{Operation: name, Authority: protocol.FromAuthority(a), Input: protocol.FromPayload(input), RequestID: request}
	switch decl.Kind {
	case protocol.OperationWait:
		var plan protocol.WaitPlan
		if err := h.conn.Call(ctx, "operation.wait", p, &plan); err != nil {
			return OperationResult{}, err
		}
		w := plan.Extension()
		return OperationResult{Output: w.Output, Wait: &w}, nil
	case protocol.OperationEmit:
		var plan protocol.EmitPlan
		if err := h.conn.Call(ctx, "operation.emit", p, &plan); err != nil {
			return OperationResult{}, err
		}
		return h.emit(ctx, a, plan)
	}
	var out protocol.OperationResult
	if err := h.conn.Call(ctx, "operation.call", p, &out); err != nil {
		return OperationResult{}, err
	}
	return OperationResult{Output: out.Output.Domain()}, nil
}

func (h *Host) emit(ctx context.Context, a extension.Authority, plan protocol.EmitPlan) (OperationResult, error) {
	e := plan.Event.Domain()
	if !slices.Contains(a.EventTypes, e.Type) || !h.ownFamily(e.Type) {
		return OperationResult{}, protocol.Errorf(protocol.CodeUndeclared, "family not declared")
	}
	if e.ID != "" || e.Source != "" || e.CausationID != "" || e.WorkID != "" || e.ExecutionID != "" || !e.ReceivedAt.IsZero() {
		return OperationResult{}, protocol.Errorf(protocol.CodeUnauthorized, "the host assigns an emission's identity, source and causation")
	}
	if e.DeliveryID == "" {
		return OperationResult{}, protocol.Errorf(protocol.CodeInvalid, "an emission needs a delivery ID")
	}
	if err := h.ext.Manifest.CheckWrite(e.Data); err != nil {
		return OperationResult{}, protocol.Errorf(protocol.CodeSchema, err.Error())
	}
	e.Source = "execution/" + string(a.ExecutionID)
	e.ID = domain.ID("emit/" + digest(string(a.ExecutionID)+"\x00"+e.DeliveryID))
	e.WorkID, e.ExecutionID = a.WorkID, a.ExecutionID
	parent := a.TriggerID
	if plan.Cause != "" {
		parent = domain.ID(plan.Cause)
	}
	h.mu.Lock()
	accepted, added := h.acceptLocked(a.ScopeID, e, parent)
	info := h.causal[accepted.Event.ID]
	h.mu.Unlock()
	emitted := extension.Emitted{Event: accepted, Added: added, Root: info.root, Depth: info.depth}
	out := OperationResult{Event: &emitted}
	if plan.CommitID == "" {
		return out, nil
	}
	var commit protocol.EmitCommit
	if err := h.conn.Call(ctx, "emit.commit", protocol.EmitCommitParams{CommitID: plan.CommitID, Emitted: protocol.FromEmitted(emitted)}, &commit); err != nil {
		return OperationResult{}, err
	}
	for _, r := range commit.Records {
		if err := h.ownNamespace(r.Namespace, r.Key); err != nil {
			return OperationResult{}, err
		}
	}
	if added {
		h.mu.Lock()
		for _, r := range commit.Records {
			h.kvPut(r.Namespace, r.Key, r.Value)
		}
		h.mu.Unlock()
	}
	out.Output = commit.Output.Domain()
	return out, nil
}

// Route asks a route handler to match one input.
func (h *Host) Route(ctx context.Context, handler string, in extension.RouteInput) (extension.RouteDecision, error) {
	var out protocol.RouteDecision
	err := h.conn.Call(ctx, "route.match", protocol.RouteMatchParams{Handler: handler, Input: protocol.FromRouteInput(in)}, &out)
	return out.Extension(), err
}

// RunTask runs a background task once.
func (h *Host) RunTask(ctx context.Context, name string) error {
	return h.conn.Call(ctx, "task.run", protocol.TaskRunParams{Task: name}, nil)
}

// Check runs a doctor check (shallow, with an empty configuration view).
func (h *Host) Check(ctx context.Context, name string) (protocol.CheckRunResult, error) {
	return h.CheckRun(ctx, protocol.CheckRunParams{Check: name})
}

// CheckRun runs a check with the given params (Deep, Config).
func (h *Host) CheckRun(ctx context.Context, p protocol.CheckRunParams) (protocol.CheckRunResult, error) {
	var out protocol.CheckRunResult
	err := h.conn.Call(ctx, "check.run", p, &out)
	return out, err
}

// Inspection.

// KV returns a copy of a namespace's values by key.
func (h *Host) KV(namespace string) map[string][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string][]byte{}
	for k, v := range h.kv[namespace] {
		out[k] = slices.Clone(v)
	}
	return out
}

// Events returns every accepted Event in order: seeded, derived, delivered
// and emitted.
func (h *Host) Events() []domain.AcceptedEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.events)
}

// Commits returns each kv.commit batch the host applied, in order.
// Refused batches (a failed check) are not included.
func (h *Host) Commits() []protocol.KVCommitParams {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.commits)
}

// Artifacts returns the committed artifacts in order.
func (h *Host) Artifacts() []Artifact {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.artifacts)
}

// Captured returns the telemetry events recorded with telemetry.capture.
func (h *Host) Captured() []protocol.TelemetryEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.captured)
}

// KVVersion is the KV layout the host has recorded (updated by Configure).
func (h *Host) KVVersion() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.kvVersion
}

// Serving the extension-to-host methods.

func unauthorized(msg string) error { return protocol.Errorf(protocol.CodeUnauthorized, msg) }
func invalid(msg string) error      { return protocol.Errorf(protocol.CodeInvalid, msg) }

var notFound = protocol.Errorf(protocol.CodeNotFound, "not found")

// Version is the KV version token the host gives a value.
func Version(value []byte) string { return digest(string(value)) }

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

func (h *Host) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	m, ok := protocol.Lookup(method)
	if !ok || m.Direction == protocol.HostToExtension {
		return nil, protocol.Errorf(protocol.CodeMethodNotFound, "no such host method")
	}
	if !h.enabled[method] {
		return nil, protocol.Errorf(protocol.CodeUndeclared, "method not enabled by the manifest")
	}
	switch method {
	case "kv.get":
		var p protocol.KVGetParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if err := h.ownNamespace(p.Namespace, p.Key); err != nil {
			return nil, err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		v, ok := h.kv[p.Namespace][p.Key]
		if !ok {
			return protocol.KVGetResult{}, nil
		}
		return protocol.KVGetResult{Found: true, Entry: protocol.KVEntry{Key: p.Key, Value: v, Version: Version(v)}}, nil
	case "kv.list":
		var p protocol.KVListParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if err := h.ownNamespace(p.Namespace, "-"); err != nil {
			return nil, err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		out := protocol.KVListResult{Entries: []protocol.KVEntry{}}
		for k, v := range h.kv[p.Namespace] {
			if strings.HasPrefix(k, p.Prefix) {
				out.Entries = append(out.Entries, protocol.KVEntry{Key: k, Value: v, Version: Version(v)})
			}
		}
		sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Key < out.Entries[j].Key })
		return out, nil
	case "kv.commit":
		var p protocol.KVCommitParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if err := h.checkBatch(p.Checks, p.Puts, p.Deletes); err != nil {
			return nil, err
		}
		if len(p.Puts)+len(p.Deletes) == 0 {
			return nil, invalid("empty batch")
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if err := h.checksLocked(p.Checks); err != nil {
			return nil, err
		}
		h.applyLocked(p.Puts, p.Deletes)
		h.commits = append(h.commits, p)
		return struct{}{}, nil
	case "state.work", "state.works", "state.executions", "state.event", "state.events", "state.scopes", "state.timers":
		var p protocol.StateParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.stateLocked(method, p)
	case "event.accept":
		var p protocol.EventAcceptParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return h.accept(p)
	case "ingress.deliver":
		var p protocol.IngressDeliverParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return h.deliver(ctx, p)
	case "secret.resolve":
		var p protocol.SecretResolveParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.allowedLocked(domain.ID(p.ScopeID)) {
			return nil, unauthorized("scope is not configured for this extension")
		}
		values, ok := h.secrets[secretKey{domain.ID(p.ScopeID), p.Connection}]
		if !ok && p.Connection != "" {
			return nil, unauthorized("connection is not bound to the extension in the scope")
		}
		out := protocol.SecretResolveResult{Values: map[string]string{}}
		for _, n := range p.Names {
			if v, ok := values[n]; ok {
				out.Values[n] = v
			}
		}
		return out, nil
	case "artifact.create", "artifact.write", "artifact.commit", "artifact.abort":
		return h.artifact(method, raw)
	case "telemetry.capture":
		var p protocol.TelemetryEvent
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if p.ScopeID != "" && !h.allowedLocked(domain.ID(p.ScopeID)) {
			return nil, unauthorized("scope is not configured for this extension")
		}
		if p.At.IsZero() {
			p.At = h.clock.Now()
		}
		h.captured = append(h.captured, p)
		return struct{}{}, nil
	}
	return nil, protocol.Errorf(protocol.CodeUnsupported, method+" is not served by sdktest")
}

func (h *Host) ownNamespace(ns, key string) error {
	if key == "" || !protocol.Namespace(h.ext.Manifest.Name, ns) {
		return unauthorized("namespace " + ns + " is not owned by " + h.ext.Manifest.Name)
	}
	return nil
}

func (h *Host) ownFamily(t string) bool {
	for _, f := range h.ext.Manifest.Provides.Families {
		if f.Type == t {
			return true
		}
	}
	return false
}

func (h *Host) checkBatch(checks []protocol.KVCheck, puts []protocol.KVPut, deletes []protocol.KVDelete) error {
	for _, c := range checks {
		if err := h.ownNamespace(c.Namespace, c.Key); err != nil {
			return err
		}
		if c.Absent == (c.Version != "") {
			return invalid("a check names a version or absence")
		}
	}
	for _, p := range puts {
		if err := h.ownNamespace(p.Namespace, p.Key); err != nil {
			return err
		}
		if p.Value == nil {
			return invalid("a put needs a value")
		}
	}
	for _, d := range deletes {
		if err := h.ownNamespace(d.Namespace, d.Key); err != nil {
			return err
		}
	}
	return nil
}

func (h *Host) checksLocked(checks []protocol.KVCheck) error {
	for _, c := range checks {
		v, ok := h.kv[c.Namespace][c.Key]
		switch {
		case !ok:
			if !c.Absent {
				return protocol.Errorf(protocol.CodeConflict, "precondition failed")
			}
		case c.Absent || Version(v) != c.Version:
			return protocol.Errorf(protocol.CodeConflict, "precondition failed")
		}
	}
	return nil
}

func (h *Host) applyLocked(puts []protocol.KVPut, deletes []protocol.KVDelete) {
	for _, p := range puts {
		h.kvPut(p.Namespace, p.Key, p.Value)
	}
	for _, d := range deletes {
		delete(h.kv[d.Namespace], d.Key)
	}
}

func (h *Host) kvPut(ns, key string, value []byte) {
	if h.kv[ns] == nil {
		h.kv[ns] = map[string][]byte{}
	}
	h.kv[ns][key] = slices.Clone(value)
}

func (h *Host) addScope(s domain.Scope) {
	if s.ID == "" || h.allowedLocked(s.ID) {
		return
	}
	h.scopes = append(h.scopes, s)
}

func (h *Host) allowedLocked(id domain.ID) bool {
	for _, s := range h.scopes {
		if s.ID == id {
			return true
		}
	}
	return false
}

// acceptLocked records e in scope, deduplicated by ID, as a root (parent
// "") or derived from parent. It reports whether e was added.
func (h *Host) acceptLocked(scope domain.ID, e domain.Event, parent domain.ID) (domain.AcceptedEvent, bool) {
	if e.ID == "" {
		e.ID = domain.ID("ext/" + digest(e.Source+"\x00"+e.DeliveryID))
	}
	for _, a := range h.events {
		if a.Event.ID == e.ID || e.DeliveryID != "" && a.Event.Source == e.Source && a.Event.DeliveryID == e.DeliveryID {
			return a, false
		}
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = h.clock.Now()
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = e.ReceivedAt
	}
	info := causal{scope: scope, root: e.ID}
	if parent != "" {
		e.CausationID = parent
		if p, ok := h.causal[parent]; ok {
			info.root, info.depth = p.root, p.depth+1
		}
	}
	a := domain.AcceptedEvent{Event: e, Sequence: uint64(len(h.events) + 1)}
	h.events = append(h.events, a)
	h.causal[e.ID] = info
	return a, true
}

func (h *Host) accept(p protocol.EventAcceptParams) (protocol.EventAcceptResult, error) {
	var out protocol.EventAcceptResult
	scope := domain.ID(p.ScopeID)
	if p.Parent == "" {
		return out, unauthorized("a subprocess never mints a causal root outside ingress")
	}
	e := p.Event.Domain()
	if !h.ownFamily(e.Type) {
		return out, protocol.Errorf(protocol.CodeUndeclared, "family not declared")
	}
	if !protocol.SourceNamespace(h.ext.Manifest.Name, e.Source) || e.DeliveryID == "" {
		return out, unauthorized("source is outside the extension's namespace")
	}
	if e.CausationID != "" || e.ExecutionID != "" {
		return out, unauthorized("causation and execution are assigned by the host")
	}
	if err := h.ext.Manifest.CheckWrite(e.Data); err != nil {
		return out, protocol.Errorf(protocol.CodeSchema, err.Error())
	}
	if err := h.checkBatch(p.Checks, p.Records, p.Deletes); err != nil {
		return out, err
	}
	e.ID = domain.ID("ext/" + digest(e.Source+"\x00"+e.DeliveryID))
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.allowedLocked(scope) {
		return out, unauthorized("scope is not configured for this extension")
	}
	parent, ok := h.causal[domain.ID(p.Parent)]
	if !ok {
		return out, unauthorized("parent is not an accepted event")
	}
	if parent.scope != scope {
		return out, unauthorized("parent is outside the scope")
	}
	if err := h.checksLocked(p.Checks); err != nil {
		return out, err
	}
	accepted, added := h.acceptLocked(scope, e, domain.ID(p.Parent))
	if added {
		h.applyLocked(p.Records, p.Deletes)
	}
	return protocol.EventAcceptResult{Event: protocol.FromAccepted(accepted), Added: added}, nil
}

// deliver runs a delivery through the source's adapter (ingress.push) and
// accepts what it observed, as the host's ingress does.
func (h *Host) deliver(ctx context.Context, p protocol.IngressDeliverParams) (protocol.IngressDeliverResult, error) {
	h.mu.Lock()
	src, ok := h.sources[p.Name]
	h.mu.Unlock()
	if !ok {
		return protocol.IngressDeliverResult{}, unauthorized("not a source this extension registered")
	}
	if p.Delivery.ReceivedAt.IsZero() {
		p.Delivery.ReceivedAt = h.Now()
	}
	var pushed protocol.PushResult
	if err := h.conn.Call(ctx, "ingress.push", protocol.IngressPushParams{Name: p.Name, Source: src.Source, Delivery: p.Delivery}, &pushed); err != nil {
		return protocol.IngressDeliverResult{}, err
	}
	r := pushed.Extension()
	events := r.Events
	for _, o := range r.Observations {
		events = append(events, o.Event)
	}
	out := protocol.IngressDeliverResult{Accepted: []protocol.AcceptedEvent{}}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range events {
		if e.Source == "" {
			e.Source = src.Source.Namespace
		}
		a, _ := h.acceptLocked(domain.ID(src.Source.ScopeID), e, "")
		out.Accepted = append(out.Accepted, protocol.FromAccepted(a))
	}
	return out, nil
}

func (h *Host) stateLocked(method string, p protocol.StateParams) (any, error) {
	switch method {
	case "state.work":
		for _, w := range h.works {
			if w.ID == domain.ID(p.ID) && h.allowedLocked(w.ScopeID) {
				return protocol.FromWork(w), nil
			}
		}
		return nil, notFound
	case "state.works":
		if p.ScopeID != "" && !h.allowedLocked(domain.ID(p.ScopeID)) {
			return nil, unauthorized("scope is not configured for this extension")
		}
		list := []protocol.Work{}
		for _, w := range h.works {
			if h.allowedLocked(w.ScopeID) && (p.ScopeID == "" || w.ScopeID == domain.ID(p.ScopeID)) {
				list = append(list, protocol.FromWork(w))
			}
		}
		return list, nil
	case "state.executions":
		var work *domain.Work
		for i := range h.works {
			if h.works[i].ID == domain.ID(p.ID) {
				work = &h.works[i]
			}
		}
		if work == nil || !h.allowedLocked(work.ScopeID) {
			return nil, notFound
		}
		list := []protocol.Execution{}
		for _, x := range h.executions {
			if x.WorkID == work.ID {
				list = append(list, protocol.FromExecution(x))
			}
		}
		return list, nil
	case "state.event":
		for _, a := range h.events {
			if a.Event.ID == domain.ID(p.ID) && h.allowedLocked(h.causal[a.Event.ID].scope) {
				return protocol.FromAccepted(a), nil
			}
		}
		return nil, notFound
	case "state.events":
		limit := p.Limit
		if limit <= 0 || limit > 1000 {
			limit = 1000
		}
		list := []protocol.AcceptedEvent{}
		for _, a := range h.events {
			if len(list) == limit {
				break
			}
			if a.Sequence > p.After && h.allowedLocked(h.causal[a.Event.ID].scope) {
				list = append(list, protocol.FromAccepted(a))
			}
		}
		return list, nil
	case "state.scopes":
		if p.ID != "" {
			for _, s := range h.scopes {
				if s.ID == domain.ID(p.ID) {
					return protocol.FromScope(s), nil
				}
			}
			return nil, notFound
		}
		list := []protocol.Scope{}
		for _, s := range h.scopes {
			list = append(list, protocol.FromScope(s))
		}
		return list, nil
	}
	return []protocol.Timer{}, nil // state.timers: sdktest arms none
}

func (h *Host) artifact(method string, raw json.RawMessage) (any, error) {
	if method == "artifact.create" {
		var p protocol.ArtifactCreateParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.calls++
		id := "writer-" + strconv.Itoa(h.calls)
		h.writers[id] = &writer{mediaType: p.MediaType}
		return protocol.ArtifactCreateResult{Writer: id}, nil
	}
	var p protocol.ArtifactWriteParams
	if err := jsonrpc.Params(raw, &p); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.writers[p.Writer]
	if w == nil {
		return nil, protocol.Errorf(protocol.CodeNotFound, "unknown writer")
	}
	switch method {
	case "artifact.write":
		if len(p.Chunk) > protocol.MaxChunkBytes {
			return nil, invalid("chunk too large")
		}
		w.data = append(w.data, p.Chunk...)
		return struct{}{}, nil
	case "artifact.commit":
		delete(h.writers, p.Writer)
		ref := domain.ArtifactRef{ID: "artifact/" + digest(fmt.Sprint(len(h.artifacts))+"\x00"+string(w.data)), Size: int64(len(w.data)), MediaType: w.mediaType}
		h.artifacts = append(h.artifacts, Artifact{Ref: ref, Data: w.data})
		return protocol.ArtifactRef{ID: ref.ID, Size: ref.Size, MediaType: ref.MediaType}, nil
	}
	delete(h.writers, p.Writer) // artifact.abort
	return struct{}{}, nil
}
