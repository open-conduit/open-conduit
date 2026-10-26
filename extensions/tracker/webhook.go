package tracker

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/extension"
)

// MaxDeliveryBytes bounds a webhook body.
const MaxDeliveryBytes = 10 << 20

// WebhookHandler serves one entry's webhook route. It verifies the delivery
// through the Service before anything else is read from it, then hands it to
// push, which must durably accept it before returning (202). It never mounts
// management APIs or forwards credentials; the listener and tunnel belong to
// the host.
func WebhookHandler(typ string, svc Service, secret func(context.Context) (string, error), push func(context.Context, extension.Delivery) error, now func() time.Time) http.Handler {
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
		d, err := VerifyDelivery(typ, svc, key, r.Header, body, now())
		switch {
		case errors.Is(err, ErrSignature):
			http.Error(w, "signature rejected", http.StatusUnauthorized)
			return
		case err != nil:
			http.Error(w, "invalid delivery", http.StatusBadRequest)
			return
		}
		if err := push(r.Context(), d); err != nil {
			// Not acknowledged: the service may redeliver, and reconcile recovers state.
			http.Error(w, "not accepted", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
}

// VerifyHMAC checks a hex HMAC-SHA256 of body (optionally prefixed
// "sha256=") in constant time. An empty secret never verifies.
func VerifyHMAC(secret string, body []byte, signature string) bool {
	if secret == "" {
		return false
	}
	sig := strings.TrimPrefix(strings.TrimSpace(signature), "sha256=")
	want, err := hex.DecodeString(sig)
	if err != nil || len(want) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// SignHMAC is the signature VerifyHMAC accepts (fixtures and fakes).
func SignHMAC(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
