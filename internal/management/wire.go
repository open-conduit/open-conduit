package management

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// Wire types use snake_case B02 names. Sequences, revisions, epochs and
// generations are decimal strings; times are UTC RFC3339Nano; optional fields
// are omitted rather than null.

type wirePayload struct {
	Schema string          `json:"schema,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}
type wireKey struct {
	Source     string `json:"source"`
	ExternalID string `json:"external_id"`
}
type wireWork struct {
	ID              string       `json:"id"`
	Kind            string       `json:"kind"`
	ScopeID         string       `json:"scope_id"`
	State           string       `json:"state"`
	CorrelationKeys []wireKey    `json:"correlation_keys"`
	Context         *wirePayload `json:"context,omitempty"`
	ParentID        string       `json:"parent_id,omitempty"`
	Display         *wireDisplay `json:"display,omitempty"`
	Revision        string       `json:"revision"`
	CreatedAt       string       `json:"created_at"`
	UpdatedAt       string       `json:"updated_at"`
}

// wireDisplay is what a Work is, as its source said; views fall back to
// the Work's own label when it is absent.
type wireDisplay struct {
	Title     string     `json:"title,omitempty"`
	Ref       string     `json:"ref,omitempty"`
	Requester string     `json:"requester,omitempty"`
	Links     []wireLink `json:"links,omitempty"`
}

// wireEventRoute is an Event's routing explanation.
type wireEventRoute struct {
	EventID      string `json:"event_id"`
	Type         string `json:"type"`
	ScopeID      string `json:"scope_id,omitempty"`
	Routed       bool   `json:"routed"`
	Disposition  string `json:"disposition,omitempty"`
	RoutedWorkID string `json:"routed_work_id,omitempty"`
	Route        string `json:"route,omitempty"`
	Binding      string `json:"binding,omitempty"`
	Generation   string `json:"generation,omitempty"`
	Policy       string `json:"policy,omitempty"`
	Reason       string `json:"reason,omitempty"`
	// DisplayRefused is why the route's Work display was dropped.
	DisplayRefused string `json:"display_refused,omitempty"`
}

type wireEvent struct {
	ID             string       `json:"id"`
	Type           string       `json:"type"`
	Source         string       `json:"source"`
	DeliveryID     string       `json:"delivery_id"`
	OccurredAt     string       `json:"occurred_at"`
	ReceivedAt     string       `json:"received_at"`
	CorrelationKey *wireKey     `json:"correlation_key,omitempty"`
	CausationID    string       `json:"causation_id,omitempty"`
	WorkID         string       `json:"work_id,omitempty"`
	ExecutionID    string       `json:"execution_id,omitempty"`
	Data           *wirePayload `json:"data,omitempty"`
	ScopeID        string       `json:"scope_id,omitempty"`
	Sequence       string       `json:"sequence"`
	Routed         bool         `json:"routed"`
	RoutedWorkID   string       `json:"routed_work_id,omitempty"`
	Disposition    string       `json:"disposition,omitempty"`
}
type wireOutcome struct {
	Kind    string       `json:"kind"`
	Summary string       `json:"summary,omitempty"`
	Data    *wirePayload `json:"data,omitempty"`
}
type wireArtifact struct {
	ID          string `json:"id"`
	Size        int64  `json:"size"`
	MediaType   string `json:"media_type,omitempty"`
	WorkID      string `json:"work_id,omitempty"`
	ExecutionID string `json:"execution_id,omitempty"`
}
type wireLease struct {
	Owner     string `json:"owner,omitempty"`
	Epoch     string `json:"epoch"`
	ExpiresAt string `json:"expires_at,omitempty"`
}
type wireExecution struct {
	ID              string         `json:"id"`
	WorkID          string         `json:"work_id"`
	ScopeID         string         `json:"scope_id,omitempty"`
	ExecutorBinding string         `json:"executor_binding"`
	Attempt         uint32         `json:"attempt"`
	State           string         `json:"state"`
	TriggerID       string         `json:"trigger_id"`
	Lease           wireLease      `json:"lease"`
	Outcome         *wireOutcome   `json:"outcome,omitempty"`
	Artifacts       []wireArtifact `json:"artifacts"`
	TraceID         string         `json:"trace_id,omitempty"`
	StartedAt       string         `json:"started_at,omitempty"`
	FinishedAt      string         `json:"finished_at,omitempty"`
	Revision        string         `json:"revision"`
	CreatedAt       string         `json:"created_at"`
	UpdatedAt       string         `json:"updated_at"`
}
type wireMatch struct {
	Keys  []wireKey `json:"keys"`
	Types []string  `json:"types"`
}
type wireWait struct {
	ID             string    `json:"id"`
	WorkID         string    `json:"work_id"`
	ExecutionID    string    `json:"execution_id"`
	State          string    `json:"state"`
	Match          wireMatch `json:"match"`
	TimeoutTimerID string    `json:"timeout_timer_id,omitempty"`
	AfterSequence  string    `json:"after_sequence"`
	WakeEventID    string    `json:"wake_event_id,omitempty"`
	Description    string    `json:"description,omitempty"`
	DueAt          string    `json:"due_at,omitempty"`
	Revision       string    `json:"revision"`
	CreatedAt      string    `json:"created_at,omitempty"`
	UpdatedAt      string    `json:"updated_at,omitempty"`
}
type wireRequires struct {
	Interrupt bool   `json:"interrupt"`
	Inject    bool   `json:"inject"`
	Resume    bool   `json:"resume"`
	Workspace bool   `json:"workspace"`
	Toolbelt  string `json:"toolbelt,omitempty"`
}
type wireBinding struct {
	Name     string       `json:"name"`
	Executor string       `json:"executor"`
	Requires wireRequires `json:"requires"`
}
type wireRoute struct {
	Name         string `json:"name"`
	Handler      string `json:"handler"`
	Binding      string `json:"binding"`
	ActivePolicy string `json:"active_policy"`
}
type wireConfig struct {
	Generation  string            `json:"generation"`
	Hash        string            `json:"hash"`
	Connections []wireConnection  `json:"connections"`
	Scopes      []wireConfigScope `json:"scopes"`
}
type wireConnection struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}
type wireConfigScope struct {
	ID         string                `json:"id"`
	Name       string                `json:"name"`
	ParentID   string                `json:"parent_id,omitempty"`
	Enabled    bool                  `json:"enabled"`
	Source     string                `json:"source,omitempty"`
	Extensions []wireConfigExtension `json:"extensions"`
	Routes     []wireConfigRoute     `json:"routes"`
	Bindings   []wireConfigBinding   `json:"bindings"`
}
type wireConfigRoute struct {
	wireRoute
	Detail string `json:"detail,omitempty"`
}
type wireEntry struct {
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
}
type wireConfigExtension struct {
	Name       string      `json:"name"`
	Type       string      `json:"type"`
	Connection string      `json:"connection,omitempty"`
	Ingress    bool        `json:"ingress"`
	Entries    []wireEntry `json:"entries,omitempty"`
}
type wireConfigBinding struct {
	Name       string `json:"name"`
	Executor   string `json:"executor"`
	Connection string `json:"connection,omitempty"`
	Resource   string `json:"resource,omitempty"`
}
type wireScope struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	ParentID   string            `json:"parent_id,omitempty"`
	Enabled    bool              `json:"enabled"`
	Bindings   []wireBinding     `json:"bindings"`
	Routes     []wireRoute       `json:"routes"`
	Config     json.RawMessage   `json:"config,omitempty"`
	SecretRefs map[string]string `json:"secret_refs"`
	Revision   string            `json:"revision"`
	CreatedAt  string            `json:"created_at"`
	UpdatedAt  string            `json:"updated_at"`
}

func u64(v uint64) string { return strconv.FormatUint(v, 10) }
func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func tsPtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return ts(*t)
}
func payload(p domain.Payload) *wirePayload {
	if p.Schema == "" && len(p.Data) == 0 {
		return nil
	}
	return &wirePayload{Schema: p.Schema, Data: p.Data}
}
func keys(ks []domain.CorrelationKey) []wireKey {
	out := make([]wireKey, 0, len(ks))
	for _, k := range ks {
		out = append(out, wireKey{k.Source, k.ExternalID})
	}
	return out
}
func toWork(w domain.Work) wireWork {
	return wireWork{ID: string(w.ID), Kind: w.Kind, ScopeID: string(w.ScopeID), State: string(w.State), CorrelationKeys: keys(w.CorrelationKeys), Context: payload(w.Context), ParentID: string(w.ParentID), Display: toDisplay(w.Display), Revision: u64(uint64(w.Revision)), CreatedAt: ts(w.CreatedAt), UpdatedAt: ts(w.UpdatedAt)}
}
func toDisplay(d *domain.WorkDisplay) *wireDisplay {
	if d == nil {
		return nil
	}
	out := &wireDisplay{Title: d.Title, Ref: d.Ref, Requester: d.Requester}
	for _, l := range d.Links {
		out.Links = append(out.Links, wireLink{l.Label, l.URL})
	}
	return out
}
func toEvent(a domain.AcceptedEvent, scope domain.ID) wireEvent {
	e := a.Event
	out := wireEvent{ID: string(e.ID), Type: e.Type, Source: e.Source, DeliveryID: e.DeliveryID, OccurredAt: ts(e.OccurredAt), ReceivedAt: ts(e.ReceivedAt), CausationID: string(e.CausationID), WorkID: string(e.WorkID), ExecutionID: string(e.ExecutionID), Data: payload(e.Data), ScopeID: string(scope), Sequence: u64(a.Sequence), Routed: a.Routed, RoutedWorkID: string(a.RoutedWorkID), Disposition: a.Disposition}
	if e.CorrelationKey != nil {
		out.CorrelationKey = &wireKey{e.CorrelationKey.Source, e.CorrelationKey.ExternalID}
	}
	return out
}
func toOutcome(o *domain.Outcome) *wireOutcome {
	if o == nil {
		return nil
	}
	return &wireOutcome{Kind: string(o.Kind), Summary: o.Summary, Data: payload(o.Data)}
}
func toExecution(x domain.Execution, scope domain.ID) wireExecution {
	out := wireExecution{ID: string(x.ID), WorkID: string(x.WorkID), ScopeID: string(scope), ExecutorBinding: x.ExecutorBinding, Attempt: x.Attempt, State: string(x.State), TriggerID: string(x.TriggerID), Lease: wireLease{Owner: x.Lease.Owner, Epoch: u64(x.Lease.Epoch), ExpiresAt: ts(x.Lease.ExpiresAt)}, Outcome: toOutcome(x.Outcome), Artifacts: []wireArtifact{}, TraceID: x.TraceID, StartedAt: tsPtr(x.StartedAt), FinishedAt: tsPtr(x.FinishedAt), Revision: u64(uint64(x.Revision)), CreatedAt: ts(x.CreatedAt), UpdatedAt: ts(x.UpdatedAt)}
	for _, a := range x.Artifacts {
		out.Artifacts = append(out.Artifacts, wireArtifact{ID: a.ID, Size: a.Size, MediaType: a.MediaType})
	}
	return out
}

// toWait renders a Wait; due is its timeout timer's fire time, zero when
// it has none or the timer has gone.
func toWait(w domain.Wait, due time.Time) wireWait {
	types := append([]string{}, w.Match.Types...)
	return wireWait{ID: string(w.ID), WorkID: string(w.WorkID), ExecutionID: string(w.ExecutionID), State: string(w.State), Match: wireMatch{Keys: keys(w.Match.Keys), Types: types}, TimeoutTimerID: string(w.TimeoutTimerID), AfterSequence: u64(w.AfterSequence), WakeEventID: string(w.WakeEventID), Description: w.Description, DueAt: ts(due), Revision: u64(uint64(w.Revision)), CreatedAt: ts(w.CreatedAt), UpdatedAt: ts(w.UpdatedAt)}
}
func toScope(s domain.Scope, enabled bool) wireScope {
	out := wireScope{ID: string(s.ID), Name: s.Name, ParentID: string(s.ParentID), Enabled: enabled, Bindings: []wireBinding{}, Routes: []wireRoute{}, SecretRefs: map[string]string{}, Revision: u64(uint64(s.Revision)), CreatedAt: ts(s.CreatedAt), UpdatedAt: ts(s.UpdatedAt)}
	for _, b := range s.Bindings {
		r := b.Requires
		out.Bindings = append(out.Bindings, wireBinding{Name: b.Name, Executor: b.Executor, Requires: wireRequires{r.Interrupt, r.Inject, r.Resume, r.Workspace, r.Toolbelt}})
	}
	for _, r := range s.Routes {
		out.Routes = append(out.Routes, wireRoute{r.Name, r.Handler, r.Binding, r.ActivePolicy})
	}
	// Reference text is sensitive even though it is not a resolved value.
	for name := range s.SecretRefs {
		out.SecretRefs[name] = "[redacted]"
	}
	if json.Valid(s.Config.Data) {
		out.Config = s.Config.Data
	}
	return out
}
