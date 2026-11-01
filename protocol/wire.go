package protocol

import (
	"encoding/json"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Wire types are the snake_case JSON shapes of protocol version 1 (07-02).
// Field names mirror the management API's wire shapes (package client) where
// those exist; revisions, sequences and epochs are JSON numbers here, since
// both sides are programs rather than browsers. Payload data is raw JSON and
// byte fields (delivery bodies, KV values, artifact chunks) are base64. The
// To/From helpers convert losslessly to the public Go contracts, so an
// adapter on either side carries exactly what an in-process call carries.

type Payload struct {
	Schema string          `json:"schema,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

func FromPayload(p domain.Payload) Payload { return Payload{Schema: p.Schema, Data: p.Data} }
func (p Payload) Domain() domain.Payload   { return domain.Payload{Schema: p.Schema, Data: p.Data} }

type Key struct {
	Source     string `json:"source"`
	ExternalID string `json:"external_id"`
}

func fromKey(k domain.CorrelationKey) Key { return Key{Source: k.Source, ExternalID: k.ExternalID} }
func (k Key) domain() domain.CorrelationKey {
	return domain.CorrelationKey{Source: k.Source, ExternalID: k.ExternalID}
}
func fromKeys(ks []domain.CorrelationKey) []Key {
	if ks == nil {
		return nil
	}
	out := make([]Key, len(ks))
	for i, k := range ks {
		out[i] = fromKey(k)
	}
	return out
}
func toKeys(ks []Key) []domain.CorrelationKey {
	if ks == nil {
		return nil
	}
	out := make([]domain.CorrelationKey, len(ks))
	for i, k := range ks {
		out[i] = k.domain()
	}
	return out
}

type Event struct {
	ID             string    `json:"id,omitempty"`
	Type           string    `json:"type"`
	Source         string    `json:"source,omitempty"`
	DeliveryID     string    `json:"delivery_id,omitempty"`
	OccurredAt     time.Time `json:"occurred_at"`
	ReceivedAt     time.Time `json:"received_at"`
	CorrelationKey *Key      `json:"correlation_key,omitempty"`
	CausationID    string    `json:"causation_id,omitempty"`
	WorkID         string    `json:"work_id,omitempty"`
	ExecutionID    string    `json:"execution_id,omitempty"`
	Data           Payload   `json:"data"`
}

func FromEvent(e domain.Event) Event {
	out := Event{ID: string(e.ID), Type: e.Type, Source: e.Source, DeliveryID: e.DeliveryID, OccurredAt: e.OccurredAt, ReceivedAt: e.ReceivedAt, CausationID: string(e.CausationID), WorkID: string(e.WorkID), ExecutionID: string(e.ExecutionID), Data: FromPayload(e.Data)}
	if e.CorrelationKey != nil {
		k := fromKey(*e.CorrelationKey)
		out.CorrelationKey = &k
	}
	return out
}
func (e Event) Domain() domain.Event {
	out := domain.Event{ID: domain.ID(e.ID), Type: e.Type, Source: e.Source, DeliveryID: e.DeliveryID, OccurredAt: e.OccurredAt, ReceivedAt: e.ReceivedAt, CausationID: domain.ID(e.CausationID), WorkID: domain.ID(e.WorkID), ExecutionID: domain.ID(e.ExecutionID), Data: e.Data.Domain()}
	if e.CorrelationKey != nil {
		k := e.CorrelationKey.domain()
		out.CorrelationKey = &k
	}
	return out
}
func FromEvents(es []domain.Event) []Event {
	if es == nil {
		return nil
	}
	out := make([]Event, len(es))
	for i, e := range es {
		out[i] = FromEvent(e)
	}
	return out
}
func ToEvents(es []Event) []domain.Event {
	if es == nil {
		return nil
	}
	out := make([]domain.Event, len(es))
	for i, e := range es {
		out[i] = e.Domain()
	}
	return out
}

type AcceptedEvent struct {
	Event        Event  `json:"event"`
	Sequence     uint64 `json:"sequence"`
	Routed       bool   `json:"routed"`
	RoutedWorkID string `json:"routed_work_id,omitempty"`
	Disposition  string `json:"disposition,omitempty"`
}

func FromAccepted(a domain.AcceptedEvent) AcceptedEvent {
	return AcceptedEvent{Event: FromEvent(a.Event), Sequence: a.Sequence, Routed: a.Routed, RoutedWorkID: string(a.RoutedWorkID), Disposition: a.Disposition}
}
func (a AcceptedEvent) Domain() domain.AcceptedEvent {
	return domain.AcceptedEvent{Event: a.Event.Domain(), Sequence: a.Sequence, Routed: a.Routed, RoutedWorkID: domain.ID(a.RoutedWorkID), Disposition: a.Disposition}
}
func FromAcceptedList(as []domain.AcceptedEvent) []AcceptedEvent {
	out := make([]AcceptedEvent, len(as))
	for i, a := range as {
		out[i] = FromAccepted(a)
	}
	return out
}

type Work struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	ScopeID         string    `json:"scope_id"`
	State           string    `json:"state"`
	CorrelationKeys []Key     `json:"correlation_keys"`
	Context         Payload   `json:"context"`
	ParentID        string    `json:"parent_id,omitempty"`
	Display         *Display  `json:"display,omitempty"`
	Revision        uint64    `json:"revision"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Display is what a Work is, in its source's words (domain.WorkDisplay,
// extension.ValidateDisplay).
type Display struct {
	Title     string        `json:"title,omitempty"`
	Ref       string        `json:"ref,omitempty"`
	Requester string        `json:"requester,omitempty"`
	Links     []DisplayLink `json:"links,omitempty"`
}
type DisplayLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

func FromDisplay(d *domain.WorkDisplay) *Display {
	if d == nil {
		return nil
	}
	out := &Display{Title: d.Title, Ref: d.Ref, Requester: d.Requester}
	for _, l := range d.Links {
		out.Links = append(out.Links, DisplayLink{Label: l.Label, URL: l.URL})
	}
	return out
}
func (d *Display) Domain() *domain.WorkDisplay {
	if d == nil {
		return nil
	}
	out := &domain.WorkDisplay{Title: d.Title, Ref: d.Ref, Requester: d.Requester}
	for _, l := range d.Links {
		out.Links = append(out.Links, domain.Link{Label: l.Label, URL: l.URL})
	}
	return out
}

func FromWork(w domain.Work) Work {
	return Work{ID: string(w.ID), Kind: w.Kind, ScopeID: string(w.ScopeID), State: string(w.State), CorrelationKeys: fromKeys(w.CorrelationKeys), Context: FromPayload(w.Context), ParentID: string(w.ParentID), Display: FromDisplay(w.Display), Revision: uint64(w.Revision), CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}
}
func (w Work) Domain() domain.Work {
	return domain.Work{ID: domain.ID(w.ID), Kind: w.Kind, ScopeID: domain.ID(w.ScopeID), State: domain.WorkState(w.State), CorrelationKeys: toKeys(w.CorrelationKeys), Context: w.Context.Domain(), ParentID: domain.ID(w.ParentID), Display: w.Display.Domain(), Revision: domain.Revision(w.Revision), CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}
}

type Lease struct {
	Owner     string    `json:"owner"`
	Epoch     uint64    `json:"epoch"`
	ExpiresAt time.Time `json:"expires_at"`
}
type LeaseRef struct {
	ExecutionID string `json:"execution_id"`
	Owner       string `json:"owner"`
	Epoch       uint64 `json:"epoch"`
}

func FromLeaseRef(l domain.LeaseRef) LeaseRef {
	return LeaseRef{ExecutionID: string(l.ExecutionID), Owner: l.Owner, Epoch: l.Epoch}
}
func (l LeaseRef) Domain() domain.LeaseRef {
	return domain.LeaseRef{ExecutionID: domain.ID(l.ExecutionID), Owner: l.Owner, Epoch: l.Epoch}
}

type Outcome struct {
	Kind    string  `json:"kind"`
	Summary string  `json:"summary,omitempty"`
	Data    Payload `json:"data"`
}

func FromOutcome(o domain.Outcome) Outcome {
	return Outcome{Kind: string(o.Kind), Summary: o.Summary, Data: FromPayload(o.Data)}
}
func (o Outcome) Domain() domain.Outcome {
	return domain.Outcome{Kind: domain.OutcomeKind(o.Kind), Summary: o.Summary, Data: o.Data.Domain()}
}

type ArtifactRef struct {
	ID        string `json:"id"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type,omitempty"`
}

func fromArtifacts(rs []domain.ArtifactRef) []ArtifactRef {
	if rs == nil {
		return nil
	}
	out := make([]ArtifactRef, len(rs))
	for i, r := range rs {
		out[i] = ArtifactRef{ID: r.ID, Size: r.Size, MediaType: r.MediaType}
	}
	return out
}
func toArtifacts(rs []ArtifactRef) []domain.ArtifactRef {
	if rs == nil {
		return nil
	}
	out := make([]domain.ArtifactRef, len(rs))
	for i, r := range rs {
		out[i] = domain.ArtifactRef{ID: r.ID, Size: r.Size, MediaType: r.MediaType}
	}
	return out
}

type Execution struct {
	ID              string        `json:"id"`
	WorkID          string        `json:"work_id"`
	ExecutorBinding string        `json:"executor_binding"`
	Attempt         uint32        `json:"attempt"`
	State           string        `json:"state"`
	TriggerID       string        `json:"trigger_id"`
	Lease           Lease         `json:"lease"`
	Outcome         *Outcome      `json:"outcome,omitempty"`
	Artifacts       []ArtifactRef `json:"artifacts,omitempty"`
	TraceID         string        `json:"trace_id,omitempty"`
	StartedAt       *time.Time    `json:"started_at,omitempty"`
	FinishedAt      *time.Time    `json:"finished_at,omitempty"`
	Revision        uint64        `json:"revision"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

func FromExecution(x domain.Execution) Execution {
	out := Execution{ID: string(x.ID), WorkID: string(x.WorkID), ExecutorBinding: x.ExecutorBinding, Attempt: x.Attempt, State: string(x.State), TriggerID: string(x.TriggerID), Lease: Lease{Owner: x.Lease.Owner, Epoch: x.Lease.Epoch, ExpiresAt: x.Lease.ExpiresAt}, Artifacts: fromArtifacts(x.Artifacts), TraceID: x.TraceID, StartedAt: x.StartedAt, FinishedAt: x.FinishedAt, Revision: uint64(x.Revision), CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt}
	if x.Outcome != nil {
		o := FromOutcome(*x.Outcome)
		out.Outcome = &o
	}
	return out
}
func (x Execution) Domain() domain.Execution {
	out := domain.Execution{ID: domain.ID(x.ID), WorkID: domain.ID(x.WorkID), ExecutorBinding: x.ExecutorBinding, Attempt: x.Attempt, State: domain.ExecutionState(x.State), TriggerID: domain.ID(x.TriggerID), Lease: domain.Lease{Owner: x.Lease.Owner, Epoch: x.Lease.Epoch, ExpiresAt: x.Lease.ExpiresAt}, Artifacts: toArtifacts(x.Artifacts), TraceID: x.TraceID, StartedAt: x.StartedAt, FinishedAt: x.FinishedAt, Revision: domain.Revision(x.Revision), CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt}
	if x.Outcome != nil {
		o := x.Outcome.Domain()
		out.Outcome = &o
	}
	return out
}

// Domain returns the bind-time capability labels (Capabilities is the
// manifest's executor capability declaration, which is also its wire shape).
func (c Capabilities) Domain() domain.Capabilities {
	return domain.Capabilities{Interrupt: c.Interrupt, Inject: c.Inject, Resume: c.Resume, Workspace: c.Workspace, Toolbelt: c.Toolbelt}
}

type Binding struct {
	Name           string         `json:"name"`
	Executor       string         `json:"executor"`
	Requires       Capabilities   `json:"requires"`
	TimeoutSeconds int            `json:"timeout_seconds,omitempty"`
	Config         Payload        `json:"config"`
	Grants         []BindingGrant `json:"grants,omitempty"`
}
type Route struct {
	Name         string  `json:"name"`
	Handler      string  `json:"handler"`
	Binding      string  `json:"binding"`
	ActivePolicy string  `json:"active_policy,omitempty"`
	Config       Payload `json:"config"`
}

func FromRoute(r domain.Route) Route {
	return Route{Name: r.Name, Handler: r.Handler, Binding: r.Binding, ActivePolicy: r.ActivePolicy, Config: FromPayload(r.Config)}
}
func (r Route) Domain() domain.Route {
	return domain.Route{Name: r.Name, Handler: r.Handler, Binding: r.Binding, ActivePolicy: r.ActivePolicy, Config: r.Config.Domain()}
}

// Scope is a scope as an extension sees it: never its secret references.
type Scope struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ParentID  string    `json:"parent_id,omitempty"`
	Bindings  []Binding `json:"bindings,omitempty"`
	Routes    []Route   `json:"routes,omitempty"`
	Config    Payload   `json:"config"`
	Revision  uint64    `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// FromScope drops SecretRefs: reference text never reaches an extension
// process through a state read (B05).
func FromScope(s domain.Scope) Scope {
	out := Scope{ID: string(s.ID), Name: s.Name, ParentID: string(s.ParentID), Config: FromPayload(s.Config), Revision: uint64(s.Revision), CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
	for _, b := range s.Bindings {
		out.Bindings = append(out.Bindings, Binding{Name: b.Name, Executor: b.Executor, Requires: Capabilities{Interrupt: b.Requires.Interrupt, Inject: b.Requires.Inject, Resume: b.Requires.Resume, Workspace: b.Requires.Workspace, Toolbelt: b.Requires.Toolbelt}, TimeoutSeconds: b.TimeoutSeconds, Config: FromPayload(b.Config), Grants: fromGrants(b.Grants)})
	}
	for _, r := range s.Routes {
		out.Routes = append(out.Routes, FromRoute(r))
	}
	return out
}
func (s Scope) Domain() domain.Scope {
	out := domain.Scope{ID: domain.ID(s.ID), Name: s.Name, ParentID: domain.ID(s.ParentID), Config: s.Config.Domain(), Revision: domain.Revision(s.Revision), CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
	for _, b := range s.Bindings {
		out.Bindings = append(out.Bindings, domain.Binding{Name: b.Name, Executor: b.Executor, Requires: b.Requires.Domain(), TimeoutSeconds: b.TimeoutSeconds, Config: b.Config.Domain(), Grants: toGrants(b.Grants)})
	}
	for _, r := range s.Routes {
		out.Routes = append(out.Routes, r.Domain())
	}
	return out
}

type Timer struct {
	ID        string    `json:"id"`
	FireAt    time.Time `json:"fire_at"`
	Event     Event     `json:"event"`
	Revision  uint64    `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func FromTimer(t domain.Timer) Timer {
	return Timer{ID: string(t.ID), FireAt: t.FireAt, Event: FromEvent(t.Event), Revision: uint64(t.Revision), CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}
func (t Timer) Domain() domain.Timer {
	return domain.Timer{ID: domain.ID(t.ID), FireAt: t.FireAt, Event: t.Event.Domain(), Revision: domain.Revision(t.Revision), CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}

// Ingress.

type Source struct {
	Namespace string `json:"namespace"`
	ScopeID   string `json:"scope_id"`
}

func FromSource(s extension.Source) Source {
	return Source{Namespace: s.Namespace, ScopeID: string(s.ScopeID)}
}
func (s Source) Extension() extension.Source {
	return extension.Source{Namespace: s.Namespace, ScopeID: domain.ID(s.ScopeID)}
}

type Delivery struct {
	ID          string    `json:"id"`
	ReceivedAt  time.Time `json:"received_at"`
	ContentType string    `json:"content_type,omitempty"`
	Body        []byte    `json:"body,omitempty"`
}

func FromDelivery(d extension.Delivery) Delivery {
	return Delivery{ID: d.ID, ReceivedAt: d.ReceivedAt, ContentType: d.ContentType, Body: d.Body}
}
func (d Delivery) Extension() extension.Delivery {
	return extension.Delivery{ID: d.ID, ReceivedAt: d.ReceivedAt, ContentType: d.ContentType, Body: d.Body}
}

type EventFamily struct {
	Type        string `json:"type"`
	ReDerivable bool   `json:"re_derivable"`
	Loss        string `json:"loss,omitempty"`
}
type Observation struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Event   Event  `json:"event"`
}

func fromObservations(os []extension.Observation) []Observation {
	if os == nil {
		return nil
	}
	out := make([]Observation, len(os))
	for i, o := range os {
		out[i] = Observation{ID: o.ID, Version: o.Version, Event: FromEvent(o.Event)}
	}
	return out
}
func toObservations(os []Observation) []extension.Observation {
	if os == nil {
		return nil
	}
	out := make([]extension.Observation, len(os))
	for i, o := range os {
		out[i] = extension.Observation{ID: o.ID, Version: o.Version, Event: o.Event.Domain()}
	}
	return out
}

type PushResult struct {
	Observations []Observation `json:"observations,omitempty"`
	Events       []Event       `json:"events,omitempty"`
}

func FromPushResult(r extension.PushResult) PushResult {
	return PushResult{Observations: fromObservations(r.Observations), Events: FromEvents(r.Events)}
}
func (r PushResult) Extension() extension.PushResult {
	return extension.PushResult{Observations: toObservations(r.Observations), Events: ToEvents(r.Events)}
}

type Scan struct {
	Source     Source `json:"source"`
	ScanID     string `json:"scan_id,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
	Checkpoint string `json:"checkpoint,omitempty"`
}

func FromScan(s extension.Scan) Scan {
	return Scan{Source: FromSource(s.Source), ScanID: s.ScanID, Cursor: s.Cursor, Checkpoint: s.Checkpoint}
}
func (s Scan) Extension() extension.Scan {
	return extension.Scan{Source: s.Source.Extension(), ScanID: s.ScanID, Cursor: s.Cursor, Checkpoint: s.Checkpoint}
}

type Snapshot struct {
	ScanID       string        `json:"scan_id"`
	Observations []Observation `json:"observations,omitempty"`
	NextCursor   string        `json:"next_cursor,omitempty"`
	Complete     bool          `json:"complete"`
	Checkpoint   string        `json:"checkpoint,omitempty"`
}

func FromSnapshot(s extension.Snapshot) Snapshot {
	return Snapshot{ScanID: s.ScanID, Observations: fromObservations(s.Observations), NextCursor: s.NextCursor, Complete: s.Complete, Checkpoint: s.Checkpoint}
}
func (s Snapshot) Extension() extension.Snapshot {
	return extension.Snapshot{ScanID: s.ScanID, Observations: toObservations(s.Observations), NextCursor: s.NextCursor, Complete: s.Complete, Checkpoint: s.Checkpoint}
}

// Routing.

type RouteInput struct {
	Event AcceptedEvent `json:"event"`
	Scope Scope         `json:"scope"`
	Work  *Work         `json:"work,omitempty"`
	Route Route         `json:"route"`
}

func FromRouteInput(in extension.RouteInput) RouteInput {
	out := RouteInput{Event: FromAccepted(in.Event), Scope: FromScope(in.Scope), Route: FromRoute(in.Route)}
	if in.Work != nil {
		w := FromWork(*in.Work)
		out.Work = &w
	}
	return out
}
func (in RouteInput) Extension() extension.RouteInput {
	out := extension.RouteInput{Event: in.Event.Domain(), Scope: in.Scope.Domain(), Route: in.Route.Domain()}
	if in.Work != nil {
		w := in.Work.Domain()
		out.Work = &w
	}
	return out
}

type RouteDecision struct {
	Match      bool   `json:"match"`
	CreateID   string `json:"create_id,omitempty"`
	Kind       string `json:"kind,omitempty"`
	AttachKeys []Key  `json:"attach_keys,omitempty"`
	Reason     string `json:"reason,omitempty"`
	// Display, on a match, replaces the Work's display; absent keeps it.
	Display *Display `json:"display,omitempty"`
	Context *Payload `json:"context,omitempty"`
	Grants  []string `json:"grants,omitempty"`
}

func fromGrants(gs []domain.BindingGrant) []BindingGrant {
	var out []BindingGrant
	for _, g := range gs {
		out = append(out, BindingGrant{Name: g.Name, Label: g.Label, Operations: g.Operations, Config: g.Config})
	}
	return out
}

func toGrants(gs []BindingGrant) []domain.BindingGrant {
	var out []domain.BindingGrant
	for _, g := range gs {
		out = append(out, domain.BindingGrant{Name: g.Name, Label: g.Label, Operations: g.Operations, Config: g.Config})
	}
	return out
}

func FromRouteDecision(d extension.RouteDecision) RouteDecision {
	out := RouteDecision{Match: d.Match, CreateID: string(d.CreateID), Kind: d.Kind, AttachKeys: fromKeys(d.AttachKeys), Reason: d.Reason, Display: FromDisplay(d.Display), Grants: d.Grants}
	if d.Context.Schema != "" || len(d.Context.Data) > 0 {
		c := FromPayload(d.Context)
		out.Context = &c
	}
	return out
}
func (d RouteDecision) Extension() extension.RouteDecision {
	out := extension.RouteDecision{Match: d.Match, CreateID: domain.ID(d.CreateID), Kind: d.Kind, AttachKeys: toKeys(d.AttachKeys), Reason: d.Reason, Display: d.Display.Domain(), Grants: d.Grants}
	if d.Context != nil {
		out.Context = d.Context.Domain()
	}
	return out
}

// RouteSample mirrors extension.RouteSample.
type RouteSample struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Event Event  `json:"event"`
}

func FromRouteSample(s extension.RouteSample) RouteSample {
	return RouteSample{ID: s.ID, Title: s.Title, Event: FromEvent(s.Event)}
}
func (s RouteSample) Extension() extension.RouteSample {
	return extension.RouteSample{ID: s.ID, Title: s.Title, Event: s.Event.Domain()}
}

// Toolbelt operations.

// Authority is the validated authority an operation receives: never the
// bearer token.
type Authority struct {
	ScopeID     string    `json:"scope_id"`
	WorkID      string    `json:"work_id"`
	ExecutionID string    `json:"execution_id"`
	Lease       LeaseRef  `json:"lease"`
	ExpiresAt   time.Time `json:"expires_at"`
	Operations  []string  `json:"operations,omitempty"`
	EventTypes  []string  `json:"event_types,omitempty"`
	TriggerID   string    `json:"trigger_id,omitempty"`
	// Connection is the connection ID the Execution's binding names, or
	// empty.
	Connection string `json:"connection,omitempty"`
}

func FromAuthority(a extension.Authority) Authority {
	return Authority{ScopeID: string(a.ScopeID), WorkID: string(a.WorkID), ExecutionID: string(a.ExecutionID), Lease: FromLeaseRef(a.Lease), ExpiresAt: a.ExpiresAt, Operations: a.Operations, EventTypes: a.EventTypes, TriggerID: string(a.TriggerID), Connection: a.Connection}
}
func (a Authority) Extension() extension.Authority {
	return extension.Authority{ScopeID: domain.ID(a.ScopeID), WorkID: domain.ID(a.WorkID), ExecutionID: domain.ID(a.ExecutionID), Lease: a.Lease.Domain(), ExpiresAt: a.ExpiresAt, Operations: a.Operations, EventTypes: a.EventTypes, TriggerID: domain.ID(a.TriggerID), Connection: a.Connection}
}

type WaitRequest struct {
	Keys          []Key      `json:"keys,omitempty"`
	Types         []string   `json:"types,omitempty"`
	Timeout       *time.Time `json:"timeout,omitempty"`
	AfterSequence uint64     `json:"after_sequence,omitempty"`
	Description   string     `json:"description,omitempty"`
}
type Dependency struct {
	ID            string   `json:"id"`
	SourceScopeID string   `json:"source_scope_id,omitempty"`
	SourceKey     Key      `json:"source_key"`
	Types         []string `json:"types,omitempty"`
	AliasType     string   `json:"alias_type"`
	Filter        Payload  `json:"filter"`
	Label         string   `json:"label,omitempty"`
	FromNow       bool     `json:"from_now,omitempty"`
}
type WaitPlan struct {
	Wait         WaitRequest  `json:"wait"`
	Dependencies []Dependency `json:"dependencies,omitempty"`
	Output       Payload      `json:"output"`
}

func FromWaitPlan(p extension.WaitPlan) WaitPlan {
	out := WaitPlan{Wait: WaitRequest{Keys: fromKeys(p.Wait.Match.Keys), Types: p.Wait.Match.Types, Timeout: p.Wait.Timeout, AfterSequence: p.Wait.AfterSequence, Description: p.Wait.Description}, Output: FromPayload(p.Output)}
	for _, d := range p.Dependencies {
		out.Dependencies = append(out.Dependencies, Dependency{ID: d.ID, SourceScopeID: string(d.SourceScopeID), SourceKey: fromKey(d.SourceKey), Types: d.Types, AliasType: d.AliasType, Filter: FromPayload(d.Filter), Label: d.Label, FromNow: d.FromNow})
	}
	return out
}
func (p WaitPlan) Extension() extension.WaitPlan {
	out := extension.WaitPlan{Wait: extension.WaitRequest{Match: domain.EventMatch{Keys: toKeys(p.Wait.Keys), Types: p.Wait.Types}, Timeout: p.Wait.Timeout, AfterSequence: p.Wait.AfterSequence, Description: p.Wait.Description}, Output: p.Output.Domain()}
	for _, d := range p.Dependencies {
		out.Dependencies = append(out.Dependencies, extension.Dependency{ID: d.ID, SourceScopeID: domain.ID(d.SourceScopeID), SourceKey: d.SourceKey.domain(), Types: d.Types, AliasType: d.AliasType, Filter: d.Filter.Domain(), Label: d.Label, FromNow: d.FromNow})
	}
	return out
}

// EmitPlan is a translated emission. CommitID, when set, names the
// extension-held commit function the host calls through emit.commit inside
// the emission transaction.
type EmitPlan struct {
	Event    Event  `json:"event"`
	Cause    string `json:"cause,omitempty"`
	Once     string `json:"once,omitempty"`
	CommitID string `json:"commit_id,omitempty"`
}
type Emitted struct {
	Event AcceptedEvent `json:"event"`
	Added bool          `json:"added"`
	Root  string        `json:"root"`
	Depth int           `json:"depth"`
}

func FromEmitted(e extension.Emitted) Emitted {
	return Emitted{Event: FromAccepted(e.Event), Added: e.Added, Root: string(e.Root), Depth: e.Depth}
}
func (e Emitted) Extension() extension.Emitted {
	return extension.Emitted{Event: e.Event.Domain(), Added: e.Added, Root: domain.ID(e.Root), Depth: e.Depth}
}

type Record struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	Value     []byte `json:"value"`
}
type EmitCommit struct {
	Records []Record `json:"records,omitempty"`
	Output  Payload  `json:"output"`
}

func FromEmitCommit(c extension.EmitCommit) EmitCommit {
	out := EmitCommit{Output: FromPayload(c.Output)}
	for _, r := range c.Records {
		out.Records = append(out.Records, Record{Namespace: r.Namespace, Key: r.Key, Value: r.Value})
	}
	return out
}
func (c EmitCommit) Extension() extension.EmitCommit {
	out := extension.EmitCommit{Output: c.Output.Domain()}
	for _, r := range c.Records {
		out.Records = append(out.Records, extension.Record{Namespace: r.Namespace, Key: r.Key, Value: r.Value})
	}
	return out
}

// Executors.

type ExecutionContext struct {
	Config           Payload           `json:"config"`
	Secrets          map[string]string `json:"secrets,omitempty"`
	Resources        bool              `json:"resources,omitempty"`
	ToolbeltEndpoint string            `json:"toolbelt_endpoint,omitempty"`
	ToolbeltToken    string            `json:"toolbelt_token,omitempty"`
	// ToolbeltTokenFile is the host-kept token file (extension.ExecutionContext).
	ToolbeltTokenFile string `json:"toolbelt_token_file,omitempty"`
	// Env is the run's environment layer (extension.ExecutionContext).
	Env map[string]string `json:"env,omitempty"`
}

// RunRequest is extension.RunRequest without its callbacks: PersistRecovery
// is execution.persist_recovery and the resource broker is resource.*, both
// addressed by the run ID the host assigns to this call.
type RunRequest struct {
	Work      Work             `json:"work"`
	Execution Execution        `json:"execution"`
	Trigger   Event            `json:"trigger"`
	Inputs    []Event          `json:"inputs,omitempty"`
	Context   ExecutionContext `json:"context"`
}

func FromRunRequest(r extension.RunRequest) RunRequest {
	return RunRequest{Work: FromWork(r.Work), Execution: FromExecution(r.Execution), Trigger: FromEvent(r.Trigger), Inputs: FromEvents(r.Inputs), Context: ExecutionContext{Config: FromPayload(r.Context.Config), Secrets: r.Context.Secrets, Resources: r.Context.Resources != nil, ToolbeltEndpoint: r.Context.ToolbeltEndpoint, ToolbeltToken: r.Context.ToolbeltToken, ToolbeltTokenFile: r.Context.ToolbeltTokenFile, Env: r.Context.Env}}
}

// Extension returns the request without callbacks; the receiving side
// installs PersistRecovery and Resources.
func (r RunRequest) Extension() extension.RunRequest {
	return extension.RunRequest{Work: r.Work.Domain(), Execution: r.Execution.Domain(), Trigger: r.Trigger.Domain(), Inputs: ToEvents(r.Inputs), Context: extension.ExecutionContext{Config: r.Context.Config.Domain(), Secrets: r.Context.Secrets, ToolbeltEndpoint: r.Context.ToolbeltEndpoint, ToolbeltToken: r.Context.ToolbeltToken, ToolbeltTokenFile: r.Context.ToolbeltTokenFile, Env: r.Context.Env}}
}

type Usage struct {
	InputTokens  *uint64  `json:"input_tokens,omitempty"`
	OutputTokens *uint64  `json:"output_tokens,omitempty"`
	CachedTokens *uint64  `json:"cached_tokens,omitempty"`
	Cost         *float64 `json:"cost,omitempty"`
	Currency     string   `json:"currency,omitempty"`
}
type WorkspaceRecord struct {
	Kind    string  `json:"kind"`
	Ref     string  `json:"ref"`
	Dir     string  `json:"dir,omitempty"`
	Summary string  `json:"summary,omitempty"`
	Data    Payload `json:"data"`
}
type Result struct {
	Outcome        Outcome          `json:"outcome"`
	Usage          Usage            `json:"usage"`
	Artifacts      []ArtifactRef    `json:"artifacts,omitempty"`
	RetryAfter     bool             `json:"retry_after,omitempty"`
	RetryResetAt   *time.Time       `json:"retry_reset_at,omitempty"`
	ConsumedInputs []string         `json:"consumed_inputs,omitempty"`
	Workspace      *WorkspaceRecord `json:"workspace,omitempty"`
}

func FromResult(r extension.Result) Result {
	out := Result{Outcome: FromOutcome(r.Outcome), Usage: Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens, CachedTokens: r.Usage.CachedTokens, Cost: r.Usage.Cost, Currency: r.Usage.Currency}, Artifacts: fromArtifacts(r.Artifacts)}
	if r.RetryHint != nil {
		out.RetryAfter, out.RetryResetAt = true, r.RetryHint.ResetAt
	}
	for _, id := range r.ConsumedInputs {
		out.ConsumedInputs = append(out.ConsumedInputs, string(id))
	}
	if r.Workspace != nil {
		out.Workspace = &WorkspaceRecord{Kind: r.Workspace.Kind, Ref: r.Workspace.Ref, Dir: r.Workspace.Dir, Summary: r.Workspace.Summary, Data: FromPayload(r.Workspace.Data)}
	}
	return out
}
func (r Result) Extension() extension.Result {
	out := extension.Result{Outcome: r.Outcome.Domain(), Usage: extension.Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens, CachedTokens: r.Usage.CachedTokens, Cost: r.Usage.Cost, Currency: r.Usage.Currency}, Artifacts: toArtifacts(r.Artifacts)}
	if r.RetryAfter {
		out.RetryHint = &extension.RetryHint{ResetAt: r.RetryResetAt}
	}
	for _, id := range r.ConsumedInputs {
		out.ConsumedInputs = append(out.ConsumedInputs, domain.ID(id))
	}
	if r.Workspace != nil {
		out.Workspace = &extension.WorkspaceRecord{Kind: r.Workspace.Kind, Ref: r.Workspace.Ref, Dir: r.Workspace.Dir, Summary: r.Workspace.Summary, Data: r.Workspace.Data.Domain()}
	}
	return out
}

type Resource struct {
	Kind    string            `json:"kind"`
	Ref     string            `json:"ref"`
	Dir     string            `json:"dir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Data    Payload           `json:"data"`
	Summary string            `json:"summary,omitempty"`
}

func FromResource(r extension.Resource) Resource {
	return Resource{Kind: r.Kind, Ref: r.Ref, Dir: r.Dir, Env: r.Env, Data: FromPayload(r.Data), Summary: r.Summary}
}
func (r Resource) Extension() extension.Resource {
	return extension.Resource{Kind: r.Kind, Ref: r.Ref, Dir: r.Dir, Env: r.Env, Data: r.Data.Domain(), Summary: r.Summary}
}

type AttachSpec struct {
	Argv []string `json:"argv"`
	Dir  string   `json:"dir,omitempty"`
}
