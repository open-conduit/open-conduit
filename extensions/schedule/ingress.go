package schedule

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
)

// WebhookLoss is the declared loss limitation of custom webhook POSTs (X04).
const WebhookLoss = "A job webhook (schedule.webhook) is an arbitrary one-shot POST with no readable backing history: reconcile cannot replay it, so a POST sent while the webhook listener or tunnel is down is lost (the sender sees the failure and may retry). Timed slots and manual runs never depend on webhooks."

// WebhookContentType wraps a POST's job, content type and body at the HTTP
// boundary, after the URL token is verified.
const WebhookContentType = "application/vnd.open-conduit.schedule-webhook+json"

// MaxWebhookBody bounds a webhook POST body.
const MaxWebhookBody = 1 << 20

// ErrCredential reports an unavailable webhook secret.
var ErrCredential = errors.New("schedule: webhook secret unavailable")

type webhookEnvelope struct {
	Job         string `json:"job"`
	ContentType string `json:"content_type,omitempty"`
	Body        []byte `json:"body,omitempty"`
}

// Ingress is the entry's push ingress: custom webhook POSTs to its jobs. Its
// only family is one-shot, so Reconcile returns an empty complete snapshot.
// Settings returns the entry's current validated settings, so editing jobs
// takes effect at the next config generation without re-registering.
type Ingress struct {
	namespace string
	settings  func() (Settings, bool)
	now       func() time.Time
}

var _ extension.Ingress = (*Ingress)(nil)

// NewIngress returns the entry's ingress.
func NewIngress(namespace string, settings func() (Settings, bool), now func() time.Time) *Ingress {
	if now == nil {
		now = time.Now
	}
	return &Ingress{namespace: namespace, settings: settings, now: now}
}

// Families declares the ingress's event families.
func Families() []extension.EventFamily {
	return []extension.EventFamily{{Type: TypeWebhook, Loss: WebhookLoss}}
}

func (g *Ingress) Spec() extension.IngressSpec { return extension.IngressSpec{Families: Families()} }

// Push normalizes one verified webhook POST into a direct one-shot Event.
func (g *Ingress) Push(_ context.Context, src extension.Source, d extension.Delivery) (extension.PushResult, error) {
	var out extension.PushResult
	if src.Namespace != g.namespace || d.ContentType != WebhookContentType || d.ID == "" || len(d.ID) > 200 {
		return out, extension.ErrInvalid
	}
	var env webhookEnvelope
	if err := strict(d.Body, &env); err != nil || len(env.Body) > MaxWebhookBody {
		return out, extension.ErrInvalid
	}
	s, ok := g.settings()
	if !ok {
		return out, extension.ErrInvalid
	}
	job, ok := s.Job(env.Job)
	if !ok || !job.Webhook() {
		return out, extension.ErrInvalid
	}
	received := d.ReceivedAt
	if received.IsZero() {
		received = g.now()
	}
	received = received.UTC()
	p := TriggerPayload{Job: job.Name, Kind: KindWebhook, Trigger: "webhook", ContentType: env.ContentType}
	if json.Valid(env.Body) {
		p.Body = append(json.RawMessage(nil), env.Body...)
	} else {
		p.Text = string(env.Body)
	}
	data, _ := json.Marshal(p)
	out.Events = []domain.Event{{ID: domain.ID("schedule-" + digest(g.namespace, "webhook", job.Name, d.ID)[:40]), Type: TypeWebhook, Source: g.namespace,
		DeliveryID: "webhook/" + job.Name + "/" + d.ID, OccurredAt: received, ReceivedAt: received,
		CorrelationKey: &domain.CorrelationKey{Source: g.namespace, ExternalID: JobKey(job.Name)},
		Data:           domain.Payload{Schema: SchemaTrigger, Data: data}}}
	return out, nil
}

// Reconcile has nothing to read: a custom POST leaves no backing history.
func (g *Ingress) Reconcile(_ context.Context, scan extension.Scan) (extension.Snapshot, error) {
	if scan.Source.Namespace != g.namespace {
		return extension.Snapshot{}, extension.ErrInvalid
	}
	return extension.Snapshot{ScanID: "none", Complete: true}, nil
}

// Token is a job's webhook URL token: an HMAC of the entry's webhook secret
// over the namespace and job, so it is stable across restarts, distinct per
// job and revoked by rotating the secret.
func Token(secret, namespace, job string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(namespace + "\x00" + job))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

var deliveryPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func newDeliveryID(now time.Time) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

// WebhookHandler serves POST <mount>/<job>/<token> for one entry (the
// listener strips its mount). It verifies the token in constant time before
// reading the body, then hands a delivery to push, which must durably accept
// it: 202 only after acceptance, 404 for an unknown job, a job without
// webhooks or a wrong token (as v1), 413 for an oversized body and 503 when
// the delivery was not accepted. A sender's Idempotency-Key (or
// X-Request-Id) makes a retried POST one delivery; otherwise each POST is
// distinct.
func WebhookHandler(namespace string, secret func(context.Context) (string, error), settings func() (Settings, bool), push func(context.Context, extension.Delivery) error, now func() time.Time) http.Handler {
	if now == nil {
		now = time.Now
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		name, token := parts[0], parts[1]
		s, ok := settings()
		job, found := s.Job(name)
		if !ok || !found || !job.Webhook() {
			http.NotFound(w, r)
			return
		}
		key, err := secret(r.Context())
		if err != nil || key == "" {
			http.Error(w, "webhook secret unavailable", http.StatusServiceUnavailable)
			return
		}
		if !hmac.Equal([]byte(token), []byte(Token(key, namespace, name))) {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, MaxWebhookBody+1))
		if err != nil {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		if len(body) > MaxWebhookBody {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		received := now()
		id := r.Header.Get("Idempotency-Key")
		if id == "" {
			id = r.Header.Get("X-Request-Id")
		}
		if !deliveryPattern.MatchString(id) {
			id = newDeliveryID(received)
		}
		envelope, _ := json.Marshal(webhookEnvelope{Job: name, ContentType: r.Header.Get("Content-Type"), Body: body})
		if err := push(r.Context(), extension.Delivery{ID: id, ReceivedAt: received, ContentType: WebhookContentType, Body: envelope}); err != nil {
			http.Error(w, "not accepted", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
}
