package extension

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// WebhookReceipt is what the host attests about one webhook request it hands
// a package: when the delivery was received and, behind a forwarder the
// operator configured (settings.webhooks.forwarder), the forwarder's own ID
// for it. The host sets it only on requests it served; it says nothing
// about the sender, whose signature the package still verifies.
type WebhookReceipt struct {
	// ReceivedAt is when the delivery was first received: by the listener
	// itself, or by the forwarder in front of it, which attested the time
	// with its MAC. A package judges a sender's signed timestamp against
	// it, so a delivery a forwarder held while the daemon was down is as
	// fresh as when it arrived.
	ReceivedAt time.Time
	// ForwardID is the forwarder's delivery ID, the same on each retry of
	// one delivery; "" when the request came straight to the listener. A
	// package whose sender gives no identity of its own can make a
	// delivery's identity from it, so a retry is one Event.
	ForwardID string
}

type webhookReceiptKey struct{}

// WithWebhookReceipt returns ctx carrying r. The host, and the SDK as it
// rebuilds a webhook request, set it; a package only reads it.
func WithWebhookReceipt(ctx context.Context, r WebhookReceipt) context.Context {
	return context.WithValue(ctx, webhookReceiptKey{}, r)
}

// WebhookReceiptFrom is the receipt the host attested for the request ctx
// belongs to, if any.
func WebhookReceiptFrom(ctx context.Context) (WebhookReceipt, bool) {
	r, ok := ctx.Value(webhookReceiptKey{}).(WebhookReceipt)
	return r, ok && !r.ReceivedAt.IsZero()
}

// WebhookReceivedAt is when the host attests r was received, in UTC, or
// now() for a request with no receipt (one a test built, or from a host
// that predates receipts). A webhook handler judges freshness against it,
// not against its own clock.
func WebhookReceivedAt(r *http.Request, now func() time.Time) time.Time {
	if rc, ok := WebhookReceiptFrom(r.Context()); ok {
		return rc.ReceivedAt.UTC()
	}
	if now == nil {
		now = time.Now
	}
	return now()
}

// WebhookForwardID is the forwarder's delivery ID for r, "" when it came
// straight to the listener.
func WebhookForwardID(r *http.Request) string {
	rc, _ := WebhookReceiptFrom(r.Context())
	return rc.ForwardID
}

// The headers a forwarder in front of the webhook listener
// (settings.webhooks.forwarder) sends with each request it forwards: when it
// first received the delivery (RFC 3339), its own ID for the delivery (the
// same on every retry; ForwardIDPattern), and its MAC (ForwardSignature).
// The host checks and removes them before a package sees the request.
const (
	ForwardedAtHeader       = "X-Open-Conduit-Forwarded-At"
	ForwardIDHeader         = "X-Open-Conduit-Forward-Id"
	ForwardSignatureHeader  = "X-Open-Conduit-Forward-Signature"
	forwardSignatureVersion = "v1"
	forwardSignaturePrefix  = forwardSignatureVersion + "="
)

// ForwardIDPattern is what a forwarder's delivery ID may be.
var ForwardIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ForwardSignature is the ForwardSignatureHeader value for one forwarded
// request: "v1=" and the hex HMAC-SHA256, keyed with the forwarder's
// secret, of
//
//	v1 \n <method> \n <target> \n <forwarded-at> \n <forward-id> \n <body>
//
// where target is the request's path and query as the listener receives
// them (the URL's RequestURI, "/slack/acme/team?x=1"), and forwarded-at and
// forward-id are the header values exactly as sent.
func ForwardSignature(secret, method, target, forwardedAt, forwardID string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	for _, part := range []string{forwardSignatureVersion, method, target, forwardedAt, forwardID} {
		mac.Write([]byte(part))
		mac.Write([]byte{'\n'})
	}
	mac.Write(body)
	return forwardSignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// VerifyForwardSignature says, in constant time, whether sig is the
// forwarder's signature of the request. An empty secret never verifies.
func VerifyForwardSignature(secret, method, target, forwardedAt, forwardID string, body []byte, sig string) bool {
	if secret == "" || !strings.HasPrefix(sig, forwardSignaturePrefix) {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(ForwardSignature(secret, method, target, forwardedAt, forwardID, body)))
}
