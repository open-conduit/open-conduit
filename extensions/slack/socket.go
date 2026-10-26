package slack

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/extension"
)

// Socket Mode receiver. The app opens an outbound WebSocket with its
// app-level token (apps.connections.open) and Slack sends Events API
// callbacks over it as envelopes, so no public endpoint is needed. An
// envelope is acknowledged only after push durably accepted its callback;
// an unacknowledged envelope is redelivered by Slack (up to three retries),
// and reconcile recovers anything still missed. Its transport (TLS to Slack
// with the app token) is the authentication boundary, as the signature is
// for HTTP.

// SocketOptions tune the receiver (zero values select defaults).
type SocketOptions struct {
	// Idle reconnects when no frame (Slack pings regularly) arrives in time.
	Idle time.Duration
	// MinBackoff and MaxBackoff bound reconnect delays after a failure.
	MinBackoff, MaxBackoff time.Duration
	Now                    func() time.Time
}

// SocketStatus is the receiver's state for doctor.
type SocketStatus struct {
	Connected  bool
	Since      time.Time // last connect or failure
	LastEvent  time.Time
	Connects   int
	LastError  string // safe code
	Acked      int
	Unacked    int // envelopes left for Slack to redeliver
	Unhandled  int // envelopes of other types (acknowledged, ignored)
	RetryUntil time.Time
}

// Socket runs one app's Socket Mode connection.
type Socket struct {
	c  *Client
	o  SocketOptions
	mu sync.Mutex
	st SocketStatus
}

// NewSocket binds a receiver to a client holding the app_token secret.
func NewSocket(c *Client, o SocketOptions) *Socket {
	if o.Idle <= 0 {
		o.Idle = 3 * time.Minute
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Socket{c: c, o: o}
}

// Status reports the receiver's state.
func (s *Socket) Status() SocketStatus { s.mu.Lock(); defer s.mu.Unlock(); return s.st }

func (s *Socket) update(f func(*SocketStatus)) { s.mu.Lock(); f(&s.st); s.mu.Unlock() }

type envelope struct {
	Type         string          `json:"type"`
	EnvelopeID   string          `json:"envelope_id"`
	Payload      json.RawMessage `json:"payload"`
	RetryAttempt int             `json:"retry_attempt"`
	Reason       string          `json:"reason"`
}

// Run connects and receives until ctx ends, reconnecting with backoff. push
// must durably accept a delivery before returning nil.
func (s *Socket) Run(ctx context.Context, push func(context.Context, extension.Delivery) error) {
	backoff := s.o.MinBackoff
	for ctx.Err() == nil {
		connected, err := s.session(ctx, push)
		if ctx.Err() != nil {
			break
		}
		if connected {
			backoff = s.o.MinBackoff
		}
		wait := backoff
		var limit *RateLimitError
		if errors.As(err, &limit) {
			if d := limit.Until.Sub(s.o.Now()); d > wait {
				wait = d
			}
		}
		code := "disconnected"
		if err != nil {
			code = socketCode(err)
		}
		s.update(func(st *SocketStatus) {
			st.Connected, st.LastError, st.Since, st.RetryUntil = false, code, s.o.Now(), s.o.Now().Add(wait)
		})
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
		if !connected {
			backoff *= 2
			if backoff > s.o.MaxBackoff {
				backoff = s.o.MaxBackoff
			}
		}
	}
	s.update(func(st *SocketStatus) { st.Connected = false })
}

func socketCode(err error) string {
	switch {
	case errors.Is(err, errClosed):
		return "closed"
	case errors.Is(err, errDisconnect):
		return "refresh"
	}
	if code := ErrorCode(err); code != "failed" {
		return code
	}
	return "connection_failed"
}

// errDisconnect is Slack's request to reconnect (a refresh or warning).
var errDisconnect = errors.New("slack socket disconnect requested")

// session runs one connection. connected reports whether hello arrived.
func (s *Socket) session(ctx context.Context, push func(context.Context, extension.Delivery) error) (bool, error) {
	u, err := s.c.OpenConnection(ctx)
	if err != nil {
		return false, err
	}
	conn, err := dialWS(ctx, u)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	connected := false
	for {
		data, err := conn.ReadText(s.o.Idle)
		if err != nil {
			return connected, err
		}
		var env envelope
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		switch env.Type {
		case "hello":
			connected = true
			s.update(func(st *SocketStatus) {
				st.Connected, st.Since, st.LastError = true, s.o.Now(), ""
				st.Connects++
			})
		case "disconnect":
			return connected, errDisconnect
		case "events_api":
			var cb struct {
				EventID string `json:"event_id"`
			}
			if env.EnvelopeID == "" || json.Unmarshal(env.Payload, &cb) != nil || !eventIDPattern.MatchString(cb.EventID) {
				continue
			}
			d := extension.Delivery{ID: cb.EventID, ReceivedAt: s.o.Now().UTC(), ContentType: DeliveryContentType, Body: append([]byte(nil), env.Payload...)}
			if err := push(ctx, d); err != nil {
				// Not acknowledged: Slack redelivers, and reconcile recovers.
				s.update(func(st *SocketStatus) { st.Unacked++ })
				continue
			}
			ack, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
			if err := conn.WriteText(ack); err != nil {
				return connected, err
			}
			s.update(func(st *SocketStatus) { st.Acked++; st.LastEvent = s.o.Now() })
		default:
			if env.EnvelopeID != "" {
				// Interactivity or commands this app does not handle: acknowledge
				// so Slack stops retrying.
				ack, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
				if err := conn.WriteText(ack); err != nil {
					return connected, err
				}
				s.update(func(st *SocketStatus) { st.Unhandled++ })
			}
		}
	}
}
