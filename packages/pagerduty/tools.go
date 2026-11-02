package pagerduty

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/storage"
)

// Operations.
const (
	OpView        = "pagerduty.incident.view"
	OpNote        = "pagerduty.incident.note"
	OpAcknowledge = "pagerduty.incident.acknowledge"
)

// CallsNS records the notes already added per request ID, so a retried
// call never adds two.
const CallsNS = "pagerduty.calls"

type tools struct{ st *state }

func refuse(code, format string, args ...any) error {
	return &extension.OperationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// refusal maps a service failure onto a safe refusal: never a token or a
// raw service body.
func refusal(err error) error {
	var op *extension.OperationError
	var api *APIError
	switch {
	case errors.As(err, &op), errors.Is(err, extension.ErrUnauthorized), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, ErrNoFrom):
		return refuse("pagerduty_no_from", "the PagerDuty connection has no from address to write as; ask the operator to set one")
	case errors.Is(err, ErrCredential):
		return refuse("pagerduty_credential", "the PagerDuty connection's token could not be used; report it as a problem")
	case errors.As(err, &api) && api.Status == 404:
		return refuse("unknown_incident", "PagerDuty has no such incident")
	case errors.As(err, &api):
		return refuse("pagerduty_refused", "PagerDuty refused the request (HTTP %d)", api.Status)
	}
	return refuse("pagerduty_unavailable", "PagerDuty could not be reached or answered with an error; try again")
}

// bound is the incident a call serves, resolved from durable state: only
// the one the calling Work was opened for.
type bound struct {
	entry    *entry
	incident string
}

func (t tools) bind(ctx context.Context, a extension.Authority) (*bound, error) {
	var w domain.Work
	if err := sdk.NewStore(sdk.HostFrom(ctx)).Transact(ctx, storage.Guard{Now: t.st.o.Now()}, func(tx storage.Tx) error {
		var err error
		w, err = tx.Work(a.WorkID)
		return err
	}); err != nil || w.ScopeID != a.ScopeID {
		return nil, extension.ErrUnauthorized
	}
	for _, e := range t.st.list() {
		if e.scope != string(w.ScopeID) {
			continue
		}
		for _, k := range w.CorrelationKeys {
			if k.Source == e.settings.Namespace && k.ExternalID != "" {
				return &bound{entry: e, incident: k.ExternalID}, nil
			}
		}
	}
	return nil, refuse("not_incident_work", "this Work was not opened for a PagerDuty incident")
}

// ViewOutput is pagerduty.incident.view's output.
type ViewOutput struct {
	IncidentPayload
	Alerts           []Alert  `json:"alerts"`
	Notes            []Note   `json:"notes"`
	AcknowledgeAllow bool     `json:"may_acknowledge"`
	Incomplete       []string `json:"incomplete,omitempty"`
}

// NoteInput is pagerduty.incident.note's input.
type NoteInput struct {
	Body string `json:"body"`
}

// Done is a changing operation's output.
type Done struct {
	Incident  string `json:"incident"`
	Status    string `json:"status,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

func (t tools) register(ext *sdk.Extension) {
	ext.Operation(OpView, sdk.Handler(t.view), sdk.Guidance(
		"Read the PagerDuty incident this Work was opened for: its status, urgency and service, its alerts (with the details the monitoring tool sent, such as a Sentry issue link) and the notes responders have added. `may_acknowledge` says whether you may acknowledge it."))
	ext.Operation(OpNote, sdk.Handler(t.note), sdk.Guidance(
		"Add a note to the PagerDuty incident this Work was opened for, for the responders: what you found, the suspect commit, the issue or pull request you opened. Notes are plain text. Give a `request_id` so a retry never adds two."))
	ext.Operation(OpAcknowledge, sdk.Handler(t.acknowledge), sdk.Guidance(
		"Acknowledge the PagerDuty incident this Work was opened for, which stops it escalating to the next responder. Only when the operator allows it (`may_acknowledge` in pagerduty.incident.view); otherwise it is refused with `not_permitted`. Never acknowledge an incident you aren't actively working."))
}

func (t tools) view(ctx context.Context, a extension.Authority, _ struct{}) (ViewOutput, error) {
	b, err := t.bind(ctx, a)
	if err != nil {
		return ViewOutput{}, err
	}
	c := b.entry.client
	i, err := c.Incident(ctx, b.incident)
	if err != nil {
		return ViewOutput{}, refusal(err)
	}
	out := ViewOutput{IncidentPayload: IncidentPayload{Incident: i.ID, Number: i.Number, Title: i.Title, Status: i.Status, Urgency: i.Urgency,
		Service: i.Service.ID, ServiceName: i.Service.Summary, URL: i.HTMLURL, CreatedAt: i.CreatedAt, StatusChanged: i.LastStatusChangeAt},
		Alerts: []Alert{}, Notes: []Note{}, AcknowledgeAllow: b.entry.settings.AllowAcknowledge}
	if alerts, err := c.Alerts(ctx, b.incident); err == nil {
		out.Alerts = alerts
	} else {
		out.Incomplete = append(out.Incomplete, "alerts")
	}
	if notes, err := c.Notes(ctx, b.incident); err == nil {
		out.Notes = notes
	} else {
		out.Incomplete = append(out.Incomplete, "notes")
	}
	return out, nil
}

func (t tools) note(ctx context.Context, a extension.Authority, in NoteInput) (Done, error) {
	if strings.TrimSpace(in.Body) == "" {
		return Done{}, refuse("bad_request", "body must not be empty")
	}
	b, err := t.bind(ctx, a)
	if err != nil {
		return Done{}, err
	}
	h := sdk.HostFrom(ctx)
	key := ""
	if id := extension.CallRequestID(ctx); id != "" {
		key = string(a.WorkID) + "/" + id
		if _, _, err := h.Get(ctx, CallsNS, key); err == nil {
			return Done{Incident: b.incident, Duplicate: true}, nil
		} else if !errors.Is(err, storage.ErrNotFound) {
			return Done{}, err
		}
	}
	if err := b.entry.client.AddNote(ctx, b.incident, in.Body); err != nil {
		return Done{}, refusal(err)
	}
	if key != "" {
		_ = h.Commit(ctx, protocol.KVCommitParams{Puts: []protocol.KVPut{{Namespace: CallsNS, Key: key, Value: []byte(b.incident)}}})
	}
	return Done{Incident: b.incident}, nil
}

func (t tools) acknowledge(ctx context.Context, a extension.Authority, _ struct{}) (Done, error) {
	b, err := t.bind(ctx, a)
	if err != nil {
		return Done{}, err
	}
	if !b.entry.settings.AllowAcknowledge {
		return Done{}, refuse("not_permitted", "the operator hasn't allowed agents to acknowledge incidents (allow_acknowledge on the pagerduty entry)")
	}
	i, err := b.entry.client.Acknowledge(ctx, b.incident)
	if err != nil {
		return Done{}, refusal(err)
	}
	return Done{Incident: b.incident, Status: i.Status}, nil
}
