package pagerduty

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
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Event families: an incident's status, observed (push and reconcile
// agree).
const (
	TypeTriggered    = "pagerduty.incident.triggered"
	TypeAcknowledged = "pagerduty.incident.acknowledged"
	TypeResolved     = "pagerduty.incident.resolved"
)

// TypeAbsent is the core's generic absence Event: a complete reconcile no
// longer lists an incident it listed before (resolved while a webhook
// was missed). Its correlation key is the incident's.
const TypeAbsent = "core.ingress.absent"

// SchemaIncident is the incident payload schema.
const SchemaIncident = "pagerduty.incident.v1"

// Families declares every ingress family.
func Families() []extension.EventFamily {
	return []extension.EventFamily{
		{Type: TypeTriggered, ReDerivable: true},
		{Type: TypeAcknowledged, ReDerivable: true},
		{Type: TypeResolved, ReDerivable: true},
	}
}

// IncidentPayload is SchemaIncident: the incident's identity and status.
type IncidentPayload struct {
	Incident      string `json:"incident"`
	Number        int    `json:"number"`
	Title         string `json:"title"`
	Status        string `json:"status"`
	Urgency       string `json:"urgency,omitempty"`
	Service       string `json:"service,omitempty"`
	ServiceName   string `json:"service_name,omitempty"`
	URL           string `json:"url,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
	StatusChanged string `json:"status_changed_at,omitempty"`
}

// MaxDeliveryBytes bounds a webhook body.
const MaxDeliveryBytes = 1 << 20

// ErrSignature is a delivery whose signature doesn't verify.
var ErrSignature = errors.New("pagerduty signature rejected")

// Verify checks a V3 webhook's X-PagerDuty-Signature: one or more
// comma-separated "v1=<hex HMAC-SHA256 of the body>" (several while a
// secret rotates), any of which may match. An empty secret never verifies.
func Verify(secret string, h http.Header, body []byte, now time.Time) (extension.Delivery, error) {
	if secret == "" {
		return extension.Delivery{}, ErrSignature
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	want := m.Sum(nil)
	ok := false
	for _, part := range strings.Split(h.Get("X-PagerDuty-Signature"), ",") {
		sig, found := strings.CutPrefix(strings.TrimSpace(part), "v1=")
		got, err := hex.DecodeString(sig)
		if found && err == nil && hmac.Equal(got, want) {
			ok = true
		}
	}
	if !ok {
		return extension.Delivery{}, ErrSignature
	}
	var env struct {
		Event struct {
			ID string `json:"id"`
		} `json:"event"`
	}
	if json.Unmarshal(body, &env) != nil || env.Event.ID == "" {
		return extension.Delivery{}, errors.New("invalid pagerduty delivery")
	}
	return extension.Delivery{ID: "pagerduty/" + env.Event.ID, ReceivedAt: now.UTC(), ContentType: "application/json", Body: body}, nil
}

// WebhookHandler verifies and pushes PagerDuty's deliveries. It fails
// closed: with no secret it answers 503, and a bad signature 401; 200 only
// after the host has durably accepted the push.
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

// Ingress observes one entry's incidents. Push reads the incident a
// verified webhook names from the API, so push and reconcile describe it
// identically; reconcile lists open incidents.
type Ingress struct {
	Settings Settings
	Client   *Client
	Now      func() time.Time
}

func (in *Ingress) Spec() extension.IngressSpec { return extension.IngressSpec{Families: Families()} }

func (in *Ingress) watches(service string) bool {
	return len(in.Settings.Services) == 0 || slices.Contains(in.Settings.Services, service)
}

// Observe is an incident's canonical observation in a namespace. Its
// version is the status and when it last changed, so an incident that is
// acknowledged and then triggered again is a new Event.
func Observe(namespace string, i Incident) (extension.Observation, bool) {
	var typ string
	switch i.Status {
	case "triggered":
		typ = TypeTriggered
	case "acknowledged":
		typ = TypeAcknowledged
	case "resolved":
		typ = TypeResolved
	default:
		return extension.Observation{}, false
	}
	if i.ID == "" {
		return extension.Observation{}, false
	}
	version := i.Status + "@" + i.LastStatusChangeAt
	data, _ := json.Marshal(IncidentPayload{Incident: i.ID, Number: i.Number, Title: i.Title, Status: i.Status, Urgency: i.Urgency,
		Service: i.Service.ID, ServiceName: i.Service.Summary, URL: i.HTMLURL, CreatedAt: i.CreatedAt, StatusChanged: i.LastStatusChangeAt})
	return extension.Observation{ID: i.ID, Version: version, Event: domain.Event{
		Type: typ, Source: namespace, DeliveryID: "incident/" + i.ID + "/" + version,
		CorrelationKey: &domain.CorrelationKey{Source: namespace, ExternalID: i.ID},
		Data:           domain.Payload{Schema: SchemaIncident, Data: data},
	}}, true
}

// Push reads the incident an incident.* delivery names; anything else
// (services, other resources) yields nothing.
func (in *Ingress) Push(ctx context.Context, src extension.Source, d extension.Delivery) (extension.PushResult, error) {
	var body struct {
		Event struct {
			EventType    string `json:"event_type"`
			ResourceType string `json:"resource_type"`
			Data         struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"data"`
		} `json:"event"`
	}
	if err := json.Unmarshal(d.Body, &body); err != nil {
		return extension.PushResult{}, extension.ErrInvalid
	}
	e := body.Event
	if e.ResourceType != "incident" || !strings.HasPrefix(e.EventType, "incident.") || e.Data.ID == "" {
		return extension.PushResult{}, nil
	}
	i, err := in.Client.Incident(ctx, e.Data.ID)
	if err != nil {
		return extension.PushResult{}, err
	}
	if !in.watches(i.Service.ID) {
		return extension.PushResult{}, nil
	}
	o, ok := Observe(src.Namespace, i)
	if !ok {
		return extension.PushResult{}, nil
	}
	return extension.PushResult{Observations: []extension.Observation{o}}, nil
}

// Reconcile reads one page of open incidents. The scan is complete on the
// last page; an incident resolved since is then absent.
func (in *Ingress) Reconcile(ctx context.Context, s extension.Scan) (extension.Snapshot, error) {
	offset := 0
	if s.Cursor != "" {
		n, err := strconv.Atoi(s.Cursor)
		if err != nil || n < 0 {
			return extension.Snapshot{}, extension.ErrInvalid
		}
		offset = n
	}
	incidents, next, err := in.Client.Incidents(ctx, in.Settings.Services, offset)
	if err != nil {
		return extension.Snapshot{}, err
	}
	scan := s.ScanID
	if scan == "" {
		scan = "scan-" + strconv.FormatInt(in.Now().UnixNano(), 36)
	}
	snap := extension.Snapshot{ScanID: scan, Complete: next < 0}
	if next >= 0 {
		snap.NextCursor = strconv.Itoa(next)
	}
	for _, i := range incidents {
		if !in.watches(i.Service.ID) {
			continue
		}
		if o, ok := Observe(s.Source.Namespace, i); ok {
			snap.Observations = append(snap.Observations, o)
		}
	}
	return snap, nil
}
