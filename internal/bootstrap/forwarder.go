package bootstrap

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/protocol"
)

// A webhook forwarder (settings.webhooks.forwarder) is something the
// operator puts in front of the listener (a queue, an API gateway, an
// ingress tier) that receives deliveries, holds them while the daemon is
// down, and forwards them when it is up. It is transport, so the host deals
// with it before any package sees a request: when one is configured the
// listener serves only requests whose forwarder MAC verifies
// (extension.ForwardSignature, over the method, path and query, body, the
// time it first received the delivery and its own delivery ID), with an
// attested receipt time no older than the forwarder's max_age_seconds and
// not in the future. The attested time and ID reach the package as its
// request's receipt (extension.WebhookReceipt), so a sender's signed
// timestamp is judged at original receipt and a retry is one delivery. The
// sender's own signature is still the package's to check: the forwarder
// attests when and that it forwarded, not what the sender said.
//
// With no forwarder configured nothing changes: a request's receipt is the
// listener's own clock, and any forwarder headers are ignored and removed.

// forwardFutureSkew is how far ahead of the daemon's clock an attested
// receipt time may be (the two clocks drift).
const forwardFutureSkew = 5 * time.Minute

// forwardRefusal is why the listener refused a forwarded request, as a
// telemetry and doctor code.
type forwardRefusal string

const (
	forwardUnsigned  forwardRefusal = "unsigned"
	forwardBadMAC    forwardRefusal = "bad_mac"
	forwardBadTime   forwardRefusal = "bad_time"
	forwardBadID     forwardRefusal = "bad_id"
	forwardStale     forwardRefusal = "stale"
	forwardFuture    forwardRefusal = "future"
	forwardTooLarge  forwardRefusal = "too_large"
	forwardNoSecret  forwardRefusal = "secret_unavailable"
	forwardTruncated forwardRefusal = "unreadable"
)

// stripForwardHeaders removes the forwarder's headers, which are the host's
// to read and never a package's.
func stripForwardHeaders(h http.Header) {
	h.Del(extension.ForwardedAtHeader)
	h.Del(extension.ForwardIDHeader)
	h.Del(extension.ForwardSignatureHeader)
}

// receive is the receipt of r: the listener's own clock with no forwarder,
// else what the forwarder f attested once its MAC verified. On refusal it
// has answered w and returns false; r's body is then read, or replaced
// with what was read.
func (d *daemon) receive(w http.ResponseWriter, r *http.Request, f *config.Forwarder) (extension.WebhookReceipt, bool) {
	if f == nil {
		stripForwardHeaders(r.Header)
		return extension.WebhookReceipt{ReceivedAt: d.now().UTC()}, true
	}
	at, id, sig := r.Header.Get(extension.ForwardedAtHeader), r.Header.Get(extension.ForwardIDHeader), r.Header.Get(extension.ForwardSignatureHeader)
	stripForwardHeaders(r.Header)
	refuse := func(code forwardRefusal, status int, msg string) (extension.WebhookReceipt, bool) {
		d.forwardRefused(r, f, code)
		http.Error(w, msg, status)
		return extension.WebhookReceipt{}, false
	}
	if at == "" || id == "" || sig == "" {
		return refuse(forwardUnsigned, http.StatusUnauthorized, "only requests from the configured forwarder are accepted")
	}
	if !extension.ForwardIDPattern.MatchString(id) {
		return refuse(forwardBadID, http.StatusUnauthorized, "forward ID is not valid")
	}
	received, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return refuse(forwardBadTime, http.StatusUnauthorized, "forwarded-at is not an RFC 3339 time")
	}
	limit := int64(protocol.MaxWebhookBodyBytes)
	if r.ContentLength > limit {
		return refuse(forwardTooLarge, http.StatusRequestEntityTooLarge, "request body too large")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	switch {
	case int64(len(body)) > limit:
		return refuse(forwardTooLarge, http.StatusRequestEntityTooLarge, "request body too large")
	case err != nil:
		return refuse(forwardTruncated, http.StatusBadRequest, "request body unreadable")
	}
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	secret, err := d.resolver.ResolveNamed(r.Context(), "forwarder", f.Secret)
	if err != nil || secret == "" {
		// The forwarder holds the delivery and retries.
		return refuse(forwardNoSecret, http.StatusServiceUnavailable, "forwarder secret unavailable")
	}
	if !extension.VerifyForwardSignature(secret, r.Method, r.URL.RequestURI(), at, id, body, sig) {
		return refuse(forwardBadMAC, http.StatusUnauthorized, "forwarder signature rejected")
	}
	// Only an authenticated time is judged: a bad MAC never says whether a
	// time would have been accepted.
	now := d.now()
	if age := now.Sub(received); age > f.MaxAge() {
		return refuse(forwardStale, http.StatusUnauthorized, "forwarded delivery was received more than "+strconv.Itoa(int(f.MaxAge()/time.Second))+" seconds ago")
	}
	if received.Sub(now) > forwardFutureSkew {
		return refuse(forwardFuture, http.StatusUnauthorized, "forwarded-at is in the future")
	}
	d.hooks.mu.Lock()
	d.hooks.forwarded++
	d.hooks.mu.Unlock()
	return extension.WebhookReceipt{ReceivedAt: received.UTC(), ForwardID: id}, true
}

// forwardRefused records a refused forwarded request: counted for doctor,
// logged, and a telemetry event, each with why and the package its path
// names, never the rest of the path (a schedule URL's token) or the request.
func (d *daemon) forwardRefused(r *http.Request, f *config.Forwarder, code forwardRefusal) {
	pkg, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	d.hooks.mu.Lock()
	d.hooks.forwardRefused++
	d.hooks.forwardLast = string(code)
	d.hooks.mu.Unlock()
	d.log.Warn("webhook request refused: not from the configured forwarder", "forwarder", f.Name, "package", pkg, "reason", string(code))
	d.capture(context.Background(), "webhooks.forward_refused", map[string]any{"forwarder": f.Name, "package": pkg, "reason": string(code)})
}
