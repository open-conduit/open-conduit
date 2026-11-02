package sentry

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Event families. An issue's state is observed (push and reconcile agree);
// an alert firing is only announced by push.
const (
	TypeOpened    = "sentry.issue.opened"
	TypeRegressed = "sentry.issue.regressed"
	TypeResolved  = "sentry.issue.resolved"
	TypeIgnored   = "sentry.issue.ignored"
	TypeAlert     = "sentry.alert.triggered"
)

// TypeAbsent is the core's generic absence Event: a complete reconcile no
// longer lists an issue it listed before (resolved or ignored while a webhook
// was missed). Its correlation key is the issue's.
const TypeAbsent = "core.ingress.absent"

// Payload schemas.
const (
	SchemaIssue = "sentry.issue.v1"
	SchemaAlert = "sentry.alert.v1"
)

// AlertLoss is the declared loss limitation of TypeAlert.
const AlertLoss = "an issue alert rule firing is only announced by webhook; an alert delivered while the daemon cannot receive it is not recovered, though reconcile still observes the issue's state"

// Families declares every ingress family.
func Families() []extension.EventFamily {
	return []extension.EventFamily{
		{Type: TypeOpened, ReDerivable: true},
		{Type: TypeRegressed, ReDerivable: true},
		{Type: TypeResolved, ReDerivable: true},
		{Type: TypeIgnored, ReDerivable: true},
		{Type: TypeAlert, Loss: AlertLoss},
	}
}

// IssuePayload is SchemaIssue: only fields that change with the issue's
// state, so a new event on the issue never changes its observation and push
// equals reconcile.
type IssuePayload struct {
	Org       string `json:"org"`
	Issue     string `json:"issue"`
	ShortID   string `json:"short_id"`
	Title     string `json:"title"`
	Culprit   string `json:"culprit,omitempty"`
	Level     string `json:"level,omitempty"`
	Status    string `json:"status"`
	Substatus string `json:"substatus,omitempty"`
	Project   string `json:"project"`
	URL       string `json:"url,omitempty"`
	FirstSeen string `json:"first_seen,omitempty"`
}

// AlertPayload is SchemaAlert.
type AlertPayload struct {
	Org     string `json:"org"`
	Issue   string `json:"issue"`
	Rule    string `json:"rule"`
	Event   string `json:"event"`
	Title   string `json:"title,omitempty"`
	Culprit string `json:"culprit,omitempty"`
	Level   string `json:"level,omitempty"`
	URL     string `json:"url,omitempty"`
}

// MaxDeliveryBytes bounds a webhook body.
const MaxDeliveryBytes = 1 << 20

// envelope is the verified delivery body the webhook hands to ingress: the
// resource Sentry named in its header and Sentry's own body.
type envelope struct {
	Resource string          `json:"resource"`
	Body     json.RawMessage `json:"body"`
}

// ErrSignature is a delivery whose signature doesn't verify.
var ErrSignature = errors.New("sentry signature rejected")

// Verify checks a delivery's Sentry-Hook-Signature (HMAC-SHA256 of the body
// with the integration's client secret, hex) and returns the delivery to
// push. An empty secret never verifies.
func Verify(secret string, h http.Header, body []byte, now time.Time) (extension.Delivery, error) {
	got, err := hex.DecodeString(h.Get("Sentry-Hook-Signature"))
	if secret == "" || err != nil || len(got) == 0 {
		return extension.Delivery{}, ErrSignature
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	if !hmac.Equal(got, m.Sum(nil)) {
		return extension.Delivery{}, ErrSignature
	}
	resource := h.Get("Sentry-Hook-Resource")
	if resource == "" || !json.Valid(body) {
		return extension.Delivery{}, errors.New("invalid sentry delivery")
	}
	id := h.Get("Request-ID")
	if id == "" {
		sum := sha256.Sum256(body)
		id = hex.EncodeToString(sum[:16])
	}
	wrapped, _ := json.Marshal(envelope{Resource: resource, Body: body})
	return extension.Delivery{ID: "sentry/" + resource + "/" + id, ReceivedAt: now.UTC(), ContentType: "application/json", Body: wrapped}, nil
}

// WebhookHandler verifies and pushes Sentry's deliveries. It fails closed:
// with no secret it answers 503, and a bad signature 401, before reading
// anything else; 200 only after the host has durably accepted the push.
func WebhookHandler(secret func(context.Context) (string, error), push func(context.Context, extension.Delivery) error, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, MaxDeliveryBytes+1))
		if err != nil || len(body) > MaxDeliveryBytes {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		key, err := secret(r.Context())
		if err != nil || key == "" {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		d, err := Verify(key, r.Header, body, now())
		switch {
		case errors.Is(err, ErrSignature):
			http.Error(w, "signature rejected", http.StatusUnauthorized)
			return
		case err != nil:
			http.Error(w, "invalid delivery", http.StatusBadRequest)
			return
		}
		if err := push(r.Context(), d); err != nil {
			http.Error(w, "not accepted", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// Ingress observes one entry's issues: push normalizes verified webhook
// deliveries and reconcile lists the organization's unresolved issues.
type Ingress struct {
	Settings Settings
	Client   *Client
	Now      func() time.Time
}

func (in *Ingress) Spec() extension.IngressSpec { return extension.IngressSpec{Families: Families()} }

func (in *Ingress) watches(project string) bool {
	return len(in.Settings.Projects) == 0 || slices.Contains(in.Settings.Projects, project)
}

// Observe is an issue's canonical observation in a namespace.
func Observe(org, namespace string, i Issue) (extension.Observation, bool) {
	var typ string
	switch {
	case i.Status == "unresolved" && i.Substatus == "regressed":
		typ = TypeRegressed
	case i.Status == "unresolved":
		typ = TypeOpened
	case i.Status == "resolved":
		typ = TypeResolved
	case i.Status == "ignored" || i.Status == "archived" || i.Status == "muted":
		typ = TypeIgnored
	default:
		return extension.Observation{}, false
	}
	if i.ID == "" {
		return extension.Observation{}, false
	}
	version := i.Status
	if i.Substatus != "" {
		version += "/" + i.Substatus
	}
	data, _ := json.Marshal(IssuePayload{Org: org, Issue: i.ID, ShortID: i.ShortID, Title: i.Title, Culprit: i.Culprit, Level: i.Level,
		Status: i.Status, Substatus: i.Substatus, Project: i.Project.Slug, URL: i.Permalink, FirstSeen: i.FirstSeen})
	return extension.Observation{ID: i.ID, Version: version, Event: domain.Event{
		Type: typ, Source: namespace, DeliveryID: "issue/" + i.ID + "/" + version,
		CorrelationKey: &domain.CorrelationKey{Source: namespace, ExternalID: i.ID},
		Data:           domain.Payload{Schema: SchemaIssue, Data: data},
	}}, true
}

// Push normalizes one verified delivery: an issue webhook is the issue's
// observation, an issue alert firing a one-shot alert Event; anything else
// (metric alerts, installation, comments) yields nothing.
func (in *Ingress) Push(_ context.Context, src extension.Source, d extension.Delivery) (extension.PushResult, error) {
	var env envelope
	if err := json.Unmarshal(d.Body, &env); err != nil {
		return extension.PushResult{}, extension.ErrInvalid
	}
	switch env.Resource {
	case "issue":
		var body struct {
			Data struct {
				Issue Issue `json:"issue"`
			} `json:"data"`
		}
		if err := json.Unmarshal(env.Body, &body); err != nil {
			return extension.PushResult{}, extension.ErrInvalid
		}
		if !in.watches(body.Data.Issue.Project.Slug) {
			return extension.PushResult{}, nil
		}
		o, ok := Observe(in.Settings.Org, src.Namespace, body.Data.Issue)
		if !ok {
			return extension.PushResult{}, nil
		}
		return extension.PushResult{Observations: []extension.Observation{o}}, nil
	case "event_alert":
		var body struct {
			Action string `json:"action"`
			Data   struct {
				Event struct {
					EventID string          `json:"event_id"`
					IssueID json.RawMessage `json:"issue_id"`
					Title   string          `json:"title"`
					Culprit string          `json:"culprit"`
					Level   string          `json:"level"`
					WebURL  string          `json:"web_url"`
				} `json:"event"`
				Rule string `json:"triggered_rule"`
			} `json:"data"`
		}
		if err := json.Unmarshal(env.Body, &body); err != nil {
			return extension.PushResult{}, extension.ErrInvalid
		}
		e := body.Data.Event
		issue := idString(e.IssueID)
		if body.Action != "triggered" || issue == "" || e.EventID == "" {
			return extension.PushResult{}, nil
		}
		data, _ := json.Marshal(AlertPayload{Org: in.Settings.Org, Issue: issue, Rule: body.Data.Rule, Event: e.EventID, Title: e.Title, Culprit: e.Culprit, Level: e.Level, URL: e.WebURL})
		return extension.PushResult{Events: []domain.Event{{
			Type: TypeAlert, Source: src.Namespace, DeliveryID: "alert/" + e.EventID + "/" + body.Data.Rule, ReceivedAt: d.ReceivedAt,
			CorrelationKey: &domain.CorrelationKey{Source: src.Namespace, ExternalID: issue},
			Data:           domain.Payload{Schema: SchemaAlert, Data: data},
		}}}, nil
	}
	return extension.PushResult{}, nil
}

// idString reads an ID Sentry sends as a string or a number.
func idString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// Reconcile reads one page of unresolved issues. The scan is complete on
// the last page; an issue that has since been resolved is then absent.
func (in *Ingress) Reconcile(ctx context.Context, s extension.Scan) (extension.Snapshot, error) {
	issues, next, err := in.Client.Issues(ctx, s.Cursor)
	if err != nil {
		return extension.Snapshot{}, err
	}
	scan := s.ScanID
	if scan == "" {
		scan = "scan-" + strconv.FormatInt(in.Now().UnixNano(), 36)
	}
	snap := extension.Snapshot{ScanID: scan, NextCursor: next, Complete: next == ""}
	for _, i := range issues {
		if !in.watches(i.Project.Slug) {
			continue
		}
		if o, ok := Observe(in.Settings.Org, s.Source.Namespace, i); ok {
			snap.Observations = append(snap.Observations, o)
		}
	}
	return snap, nil
}
