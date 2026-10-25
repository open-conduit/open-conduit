package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/extension"
)

// ErrSignature means a delivery failed HMAC verification. Its body is never
// parsed or retained.
var ErrSignature = errors.New("github delivery signature rejected")

// MaxDeliveryBytes bounds a webhook body (GitHub caps payloads at 25 MiB).
const MaxDeliveryBytes = 25 << 20

var (
	eventName = regexp.MustCompile(`^[a-z_]{1,64}$`)
	guid      = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)
)

// VerifySignature checks X-Hub-Signature-256 (HMAC-SHA256 of the raw body) in
// constant time. An empty secret never verifies.
func VerifySignature(secret string, body []byte, header string) bool {
	if secret == "" {
		return false
	}
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	want, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// VerifyDelivery authenticates a raw webhook request and wraps it into the
// Delivery that Push accepts. This is the transport-authentication boundary;
// Push itself trusts its caller.
func VerifyDelivery(secret string, h http.Header, body []byte, received time.Time) (extension.Delivery, error) {
	if !VerifySignature(secret, body, h.Get("X-Hub-Signature-256")) {
		return extension.Delivery{}, ErrSignature
	}
	event, id := h.Get("X-GitHub-Event"), h.Get("X-GitHub-Delivery")
	if !eventName.MatchString(event) || !guid.MatchString(id) || !json.Valid(body) {
		return extension.Delivery{}, extension.ErrInvalid
	}
	wrapped, err := json.Marshal(envelope{Event: event, Delivery: id, Payload: body})
	if err != nil {
		return extension.Delivery{}, extension.ErrInvalid
	}
	return extension.Delivery{ID: id, ReceivedAt: received.UTC(), ContentType: DeliveryContentType, Body: wrapped}, nil
}

// WebhookHandler serves one repository's webhook route. It verifies the
// signature before parsing, then hands the delivery to push, which must
// durably accept it before returning (202). It never mounts management APIs
// or forwards credentials; listener and tunnel exposure belong to the host.
func WebhookHandler(secret func(context.Context) (string, error), push func(context.Context, extension.Delivery) error, now func() time.Time) http.Handler {
	if now == nil {
		now = time.Now
	}
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
		if err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		d, err := VerifyDelivery(key, r.Header, body, now())
		switch {
		case errors.Is(err, ErrSignature):
			http.Error(w, "signature rejected", http.StatusUnauthorized)
			return
		case err != nil:
			http.Error(w, "invalid delivery", http.StatusBadRequest)
			return
		}
		if err := push(r.Context(), d); err != nil {
			// Not acknowledged: GitHub may redeliver, and reconcile recovers state.
			http.Error(w, "not accepted", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
}
