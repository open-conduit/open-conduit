package slack

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
	"strconv"
	"time"

	"github.com/open-conduit/open-conduit/extension"
)

// DeliveryContentType marks a Delivery body VerifyDelivery authenticated: an
// Events API `event_callback` envelope, unchanged.
const DeliveryContentType = "application/vnd.open-conduit.slack-event+json"

// ErrSignature means a delivery failed signature or freshness verification.
// Its body is never parsed or retained.
var ErrSignature = errors.New("slack delivery signature rejected")

// MaxDeliveryBytes bounds an Events API body.
const MaxDeliveryBytes = 4 << 20

// MaxSkew is how far a request timestamp may be from receipt (replay guard).
const MaxSkew = 5 * time.Minute

var eventIDPattern = regexp.MustCompile(`^Ev[A-Za-z0-9]{1,64}$`)

// VerifySignature checks X-Slack-Signature: v0= HMAC-SHA256 of
// "v0:<timestamp>:<body>" with the signing secret, in constant time. An empty
// secret never verifies.
func VerifySignature(secret, timestamp string, body []byte, header string) bool {
	if secret == "" || timestamp == "" || len(header) < 4 || header[:3] != "v0=" {
		return false
	}
	want, err := hex.DecodeString(header[3:])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + timestamp + ":"))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// Sign returns the X-Slack-Signature for body at timestamp (test and fake
// support; it is also the exact inverse of VerifySignature).
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + timestamp + ":"))
	mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// Verified is an authenticated Events API request: either a delivery for
// Push, a URL-verification challenge to echo, or neither (a callback type
// that needs only acknowledgement, such as app_rate_limited).
type Verified struct {
	Delivery  extension.Delivery
	Challenge string
}

// VerifyDelivery authenticates a raw Events API request (signature and a
// fresh timestamp) before parsing it. This is the transport-authentication
// boundary; Push itself trusts its caller.
func VerifyDelivery(secret string, h http.Header, body []byte, received time.Time) (Verified, error) {
	ts := h.Get("X-Slack-Request-Timestamp")
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || !VerifySignature(secret, ts, body, h.Get("X-Slack-Signature")) {
		return Verified{}, ErrSignature
	}
	if skew := received.Sub(time.Unix(secs, 0)); skew > MaxSkew || skew < -MaxSkew {
		return Verified{}, ErrSignature
	}
	var env struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		EventID   string `json:"event_id"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Verified{}, extension.ErrInvalid
	}
	switch env.Type {
	case "url_verification":
		if env.Challenge == "" || len(env.Challenge) > 256 {
			return Verified{}, extension.ErrInvalid
		}
		return Verified{Challenge: env.Challenge}, nil
	case "event_callback":
		if !eventIDPattern.MatchString(env.EventID) {
			return Verified{}, extension.ErrInvalid
		}
		return Verified{Delivery: extension.Delivery{ID: env.EventID, ReceivedAt: received.UTC(), ContentType: DeliveryContentType, Body: append([]byte(nil), body...)}}, nil
	}
	return Verified{}, nil
}

// Routed is what a webhook route made of a verified callback.
type Routed struct {
	// Entries is how many configured entries the callback was for, each of
	// which accepted it. None is answered 421 Misdirected Request.
	Entries int
	// Duplicate is true when every one of them had already accepted it
	// (Slack's retry of a callback).
	Duplicate bool
}

// receipt is the JSON body of a 200 for a callback.
type receipt struct {
	Entries   int  `json:"entries"`
	Duplicate bool `json:"duplicate"`
}

// WebhookHandler serves one entry's Events API Request URL. It verifies the
// signature before parsing, echoes URL verification, and hands a callback to
// push, which must durably accept it before returning (200). A failure
// answers 503 so Slack retries; reconcile recovers what retries miss. It
// never mounts management APIs or forwards credentials.
func WebhookHandler(secret func(context.Context) (string, error), push func(context.Context, extension.Delivery) error, now func() time.Time) http.Handler {
	return RoutedHandler(secret, func(ctx context.Context, d extension.Delivery) (Routed, error) {
		return Routed{Entries: 1}, push(ctx, d)
	}, now)
}

// RoutedHandler serves an Events API Request URL whose callbacks route
// decides among entries. It verifies the signature before parsing (401
// before any entry sees the callback) and echoes URL verification, then
// hands a callback to route, which must durably accept it in every entry it
// is for before returning. It answers 200 with a receipt ({"entries": n,
// "duplicate": bool}) once every entry accepted; 421 when the callback is
// for no entry, with X-Slack-No-Retry so Slack doesn't retry a misroute;
// and 503 when an entry didn't accept it, so Slack retries and the entries
// that had accepted deduplicate.
func RoutedHandler(secret func(context.Context) (string, error), route func(context.Context, extension.Delivery) (Routed, error), now func() time.Time) http.Handler {
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
		v, err := VerifyDelivery(key, r.Header, body, extension.WebhookReceivedAt(r, now))
		switch {
		case errors.Is(err, ErrSignature):
			http.Error(w, "signature rejected", http.StatusUnauthorized)
			return
		case err != nil:
			http.Error(w, "invalid delivery", http.StatusBadRequest)
			return
		}
		if v.Challenge != "" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, v.Challenge)
			return
		}
		if v.Delivery.ID == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		routed, err := route(r.Context(), v.Delivery)
		switch {
		case err != nil:
			http.Error(w, "not accepted", http.StatusServiceUnavailable)
			return
		case routed.Entries == 0:
			w.Header().Set("X-Slack-No-Retry", "1")
			http.Error(w, "no configured entry is for this callback", http.StatusMisdirectedRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(receipt{Entries: routed.Entries, Duplicate: routed.Duplicate})
	})
}
