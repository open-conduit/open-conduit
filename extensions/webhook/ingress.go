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

// Push turns one verified POST into a delivery Event. Its delivery ID is
// the hook and the delivery key's digest (or a fresh ID for a hook without
// one), so the core's (source, delivery ID) deduplication makes a retry
// the first delivery again. Its correlation key is the hook and the
// correlation value's digest, or the delivery's own ID.
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
	p := Payload{Hook: hook.Name, Delivery: env.Delivery, Correlation: env.Correlation, ContentType: env.ContentType}
	if json.Valid(env.Body) {
		p.Body = append(json.RawMessage(nil), env.Body...)
	} else {
		p.Text = string(env.Body)
	}
	data, _ := json.Marshal(p)
	out.Events = []domain.Event{{ID: domain.ID("webhook-" + digest(in.Namespace, hook.Name, d.ID)[:40]), Type: TypeDelivery, Source: in.Namespace,
		DeliveryID: hook.Name + "/" + d.ID, OccurredAt: received, ReceivedAt: received,
		CorrelationKey: &domain.CorrelationKey{Source: in.Namespace, ExternalID: Thread(hook.Name, env.Correlation, d.ID)},
		Data:           domain.Payload{Schema: SchemaDelivery, Data: data}}}
	return out, nil
}

// Thread is a delivery's correlation external ID: the hook and the
// correlation value's digest, or the hook and the delivery's own ID.
func Thread(hook, correlation, delivery string) string {
	if correlation != "" {
		return hook + "/c/" + digest(correlation)[:32]
	}
	return hook + "/d/" + delivery
}

// Reconcile has nothing to read: a POST leaves no backing history.
func (in *Ingress) Reconcile(_ context.Context, scan extension.Scan) (extension.Snapshot, error) {
	if scan.Source.Namespace != in.Namespace {
		return extension.Snapshot{}, extension.ErrInvalid
	}
	return extension.Snapshot{ScanID: "none", Complete: true}, nil
}

// Token is a hook's URL token: an HMAC of the entry's webhook secret over
// the namespace and hook, so it is stable across restarts, distinct per
// hook and revoked by rotating the secret.
func Token(secret, namespace, hook string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(namespace + "\x00" + hook))
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
// unknown hook or a wrong token, 413 for a body over 1 MiB, 400 for a
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
		if !hmac.Equal([]byte(parts[1]), []byte(Token(key, namespace, hook.Name))) {
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
		raw, _ := json.Marshal(env)
		if err := push(r.Context(), extension.Delivery{ID: id, ReceivedAt: received, ContentType: ContentType, Body: raw}); err != nil {
			http.Error(w, "not accepted", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
}
