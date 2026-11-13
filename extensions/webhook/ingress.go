package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// TypeDelivery is the Event family of an accepted POST.
const TypeDelivery = "webhook.delivery"

// SchemaDelivery is its payload schema.
const SchemaDelivery = "webhook.delivery.v1"

// Loss is the declared loss limitation of a webhook POST.
const Loss = "A webhook delivery (webhook.delivery) is an arbitrary one-shot POST with no readable backing history: reconcile cannot replay it, so a POST sent while the webhook listener or tunnel is down is lost (the sender sees the failure and may retry)."

// ContentType wraps a POST's hook, keys, content type and body at the HTTP
// boundary, once the URL token has been verified.
const ContentType = "application/vnd.open-conduit.webhook-delivery+json"

// MaxBody bounds a POST body.
const MaxBody = 1 << 20

// MaxEncoded bounds a delivery's Events together, encoded: each target's
// Event carries the whole body, and JSON can escape a byte as six (<, >, &
// and control characters become \u00XX), so a fan-out of an escape-heavy
// body could outgrow the host's ingress page (8 MiB by default) and never
// be accepted. A delivery over it is refused with 413. A hook without
// fan-out is never over it: its one Event of a 1 MiB body is at most 6 MiB.
const MaxEncoded = 7 << 20

// MaxKey bounds a delivery or correlation key's value.
const MaxKey = 256

// ErrCredential reports an unavailable webhook secret.
var ErrCredential = errors.New("webhook: webhook secret unavailable")

// errNoKey is a configured key the delivery doesn't carry.
var errNoKey = errors.New("missing")

// Families declares the ingress's one family.
func Families() []extension.EventFamily {
	return []extension.EventFamily{{Type: TypeDelivery, Loss: Loss}}
}

// Payload is SchemaDelivery.
type Payload struct {
	Hook string `json:"hook"`
	// Target is the fan-out target this Event is for, when the hook fans
	// out.
	Target string `json:"target,omitempty"`
	// Delivery is the delivery key's value, when the hook has one.
	Delivery string `json:"delivery,omitempty"`
	// Correlation is the correlation key's value, when the delivery
	// carries one.
	Correlation string `json:"correlation,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	// Body is a JSON body; Text is any other body.
	Body json.RawMessage `json:"body,omitempty"`
	Text string          `json:"text,omitempty"`
}

// envelope is what the handler hands ingress: the keys were read from the
// request, so the headers need not travel.
type envelope struct {
	Hook        string `json:"hook"`
	Delivery    string `json:"delivery,omitempty"`
	Correlation string `json:"correlation,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Body        []byte `json:"body,omitempty"`
}

// Ingress is an entry's push ingress. Its only family is one-shot, so
// Reconcile returns an empty complete snapshot.
type Ingress struct {
	Namespace string
	Settings  Settings
	Now       func() time.Time
}

var _ extension.Ingress = (*Ingress)(nil)

func (in *Ingress) Spec() extension.IngressSpec { return extension.IngressSpec{Families: Families()} }

// Push turns one verified POST into a delivery Event, or into one Event
// for each target of a hook that fans out. Its delivery ID is the hook (and
// target) and the delivery key's digest (or a fresh ID for a hook without
// one), so the core's (source, delivery ID) deduplication makes a retry
// the first delivery again, target by target. Its correlation key is the
// hook (and target) and the correlation value's digest, or the delivery's
// own ID, so each target threads on its own. The host accepts every Event
// of a delivery in one transaction, so the sender's 202 covers them all.
func (in *Ingress) Push(_ context.Context, src extension.Source, d extension.Delivery) (extension.PushResult, error) {
	var out extension.PushResult
	if src.Namespace != in.Namespace || d.ContentType != ContentType || d.ID == "" || len(d.ID) > 200 {
		return out, extension.ErrInvalid
	}
	var env envelope
	if err := strictJSON(d.Body, &env); err != nil || len(env.Body) > MaxBody || len(env.Delivery) > MaxKey || len(env.Correlation) > MaxKey {
		return out, extension.ErrInvalid
	}
	hook, ok := in.Settings.Hook(env.Hook)
	if !ok {
		return out, extension.ErrInvalid
	}
	dk, _ := hook.Keys()
	if dk.Set() != (env.Delivery != "") {
		return out, extension.ErrInvalid
	}
	received := d.ReceivedAt
	if received.IsZero() {
		received = in.Now()
	}
	received = received.UTC()
	targets := hook.FanOut
	if len(targets) == 0 {
		targets = []string{""}
	}
	if encodedSize(hook, env) > MaxEncoded {
		return out, extension.ErrInvalid
	}
	for _, target := range targets {
		data := env.payload(target)
		lane, id := hook.Name, digest(in.Namespace, hook.Name, d.ID)
		if target != "" {
			lane, id = hook.Name+"/"+target, digest(in.Namespace, hook.Name, target, d.ID)
		}
		out.Events = append(out.Events, domain.Event{ID: domain.ID("webhook-" + id[:40]), Type: TypeDelivery, Source: in.Namespace,
			DeliveryID: lane + "/" + d.ID, OccurredAt: received, ReceivedAt: received,
			CorrelationKey: &domain.CorrelationKey{Source: in.Namespace, ExternalID: Thread(hook.Name, target, env.Correlation, d.ID)},
			Data:           domain.Payload{Schema: SchemaDelivery, Data: data}})
	}
	return out, nil
}

// payload is the delivery's payload for one target, encoded.
func (env envelope) payload(target string) []byte {
	p := Payload{Hook: env.Hook, Target: target, Delivery: env.Delivery, Correlation: env.Correlation, ContentType: env.ContentType}
	if json.Valid(env.Body) {
		p.Body = append(json.RawMessage(nil), env.Body...)
	} else {
		p.Text = string(env.Body)
	}
	data, _ := json.Marshal(p)
	return data
}

// eventOverhead bounds what an Event adds to its encoded payload (IDs,
// source, times, correlation key).
const eventOverhead = 1 << 10

// encodedSize is the encoded size of the Events a delivery to the hook
// becomes: one for each target, or one.
func encodedSize(hook Hook, env envelope) int {
	n := 0
	targets := hook.FanOut
	if len(targets) == 0 {
		targets = []string{""}
	}
	for _, t := range targets {
		n += len(env.payload(t)) + eventOverhead
	}
	return n
}

// Thread is a delivery's correlation external ID: the hook, the fan-out
// target when there is one, and the correlation value's digest or else
// the delivery's own ID.
func Thread(hook, target, correlation, delivery string) string {
	lane := hook
	if target != "" {
		lane += "/" + target
	}
	if correlation != "" {
		return lane + "/c/" + digest(correlation)[:32]
	}
	return lane + "/d/" + delivery
}

// Reconcile has nothing to read: a POST leaves no backing history.
func (in *Ingress) Reconcile(_ context.Context, scan extension.Scan) (extension.Snapshot, error) {
	if scan.Source.Namespace != in.Namespace {
		return extension.Snapshot{}, extension.ErrInvalid
	}
	return extension.Snapshot{ScanID: "none", Complete: true}, nil
}

// Token is a hook's URL token: an HMAC of the entry's webhook secret over
// the namespace, the hook and its version, so it is stable across
// restarts and distinct per hook. Rotating the secret revokes every hook's
// URL; bumping a hook's version revokes that hook's alone. Version 0 signs
// only the namespace and hook, so a hook's first URL is the same as it was
// before versions existed.
func Token(secret, namespace, hook string, version int) string {
	mac := hmac.New(sha256.New, []byte(secret))
	msg := namespace + "\x00" + hook
	if version != 0 {
		msg += "\x00" + strconv.Itoa(version)
	}
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// digest is a hex SHA-256 of the parts, each length-prefixed.
func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func newDeliveryID(now time.Time) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "p-" + now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

// Value reads a key from a request: the header's value, or the string,
// number or boolean the pointer names in a JSON body. Anything else (no
// such header or member, an object, an array, null, a body that isn't
// JSON) is errNoKey; a value that is too long or not UTF-8 is an error.
func (k Key) Value(h http.Header, body []byte) (string, error) {
	var v string
	if k.Header != "" {
		v = strings.TrimSpace(h.Get(k.Header))
	} else {
		parts, _ := splitPointer(k.Pointer)
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		var doc any
		if dec.Decode(&doc) != nil {
			return "", errNoKey
		}
		for _, p := range parts {
			switch node := doc.(type) {
			case map[string]any:
				doc = node[p]
			case []any:
				i, err := strconv.Atoi(p)
				if err != nil || i < 0 || i >= len(node) || (len(p) > 1 && p[0] == '0') {
					return "", errNoKey
				}
				doc = node[i]
			default:
				return "", errNoKey
			}
		}
		switch x := doc.(type) {
		case string:
			v = x
		case json.Number:
			v = x.String()
		case bool:
			v = strconv.FormatBool(x)
		}
	}
	if v == "" {
		return "", errNoKey
	}
	if len(v) > MaxKey || !utf8.ValidString(v) {
		return "", errors.New("too long")
	}
	return v, nil
}

// splitPointer parses an RFC 6901 JSON pointer into its unescaped tokens.
// The whole document ("") is not a key.
func splitPointer(p string) ([]string, error) {
	if !strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("%q is not a JSON pointer (it starts with /)", p)
	}
	parts := strings.Split(p[1:], "/")
	for i, s := range parts {
		for j := 0; j < len(s); j++ {
			if s[j] == '~' && (j+1 >= len(s) || (s[j+1] != '0' && s[j+1] != '1')) {
				return nil, fmt.Errorf("%q has a bad ~ escape", p)
			}
		}
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
	}
	return parts, nil
}

// WebhookHandler serves POST /<hook>/<token> for one entry (the caller
// strips the entry's mount). It checks the token in constant time before
// reading the body, reads the hook's keys, then hands push the delivery,
// which must accept it durably: 202 only after acceptance, 404 for an
// unknown hook or a wrong token, 413 for a body over 1 MiB (or one whose
// fan-out Events would together encode over MaxEncoded), 400 for a
// delivery without the hook's delivery key or with a key over 256 bytes,
// and 503 when the secret is unavailable or the delivery wasn't accepted.
func WebhookHandler(namespace string, s Settings, secret func(context.Context) (string, error), push func(context.Context, extension.Delivery) error, now func() time.Time) http.Handler {
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
		hook, found := s.Hook(parts[0])
		if !found {
			http.NotFound(w, r)
			return
		}
		key, err := secret(r.Context())
		if err != nil || key == "" {
			http.Error(w, "webhook secret unavailable", http.StatusServiceUnavailable)
			return
		}
		if !hmac.Equal([]byte(parts[1]), []byte(Token(key, namespace, hook.Name, hook.Version))) {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
		if err != nil {
			http.Error(w, "unreadable body", http.StatusBadRequest)
			return
		}
		if len(body) > MaxBody {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		received := now()
		dk, ck := hook.Keys()
		env := envelope{Hook: hook.Name, ContentType: r.Header.Get("Content-Type"), Body: body}
		id := newDeliveryID(received)
		if dk.Set() {
			v, err := dk.Value(r.Header, body)
			if err != nil {
				http.Error(w, "delivery key "+hook.DeliveryKey+": "+err.Error(), http.StatusBadRequest)
				return
			}
			env.Delivery, id = v, "k-"+digest(v)[:40]
		}
		if ck.Set() {
			v, err := ck.Value(r.Header, body)
			if err != nil && !errors.Is(err, errNoKey) {
				http.Error(w, "correlation key "+hook.CorrelationKey+": "+err.Error(), http.StatusBadRequest)
				return
			}
			env.Correlation = v
		}
		if encodedSize(hook, env) > MaxEncoded {
			http.Error(w, fmt.Sprintf("body too large for %d fan-out targets", len(hook.FanOut)), http.StatusRequestEntityTooLarge)
			return
		}
		raw, _ := json.Marshal(env)
		if err := push(r.Context(), extension.Delivery{ID: id, ReceivedAt: received, ContentType: ContentType, Body: raw}); err != nil {
			http.Error(w, "not accepted", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
}
