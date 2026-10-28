package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/tracker"
)

// Route is the deterministic `bitbucket` route handler: it selects input for
// existing Work and never creates Work (a PR belongs to its tracker issue's
// Work; an unowned PR is only observed). Input: closes and merges, finished
// checks, human comments and other agents' comments; never edits, bot
// comments, this Work's own notes, open-state updates or pending checks.
type Route struct{}

var _ extension.RouteHandler = Route{}

func settingsFor(scope domain.Scope, source string) (Settings, bool) {
	var meta struct {
		Extensions []struct {
			Type   string          `json:"type"`
			Config json.RawMessage `json:"config"`
		} `json:"extensions"`
	}
	if len(scope.Config.Data) == 0 || json.Unmarshal(scope.Config.Data, &meta) != nil {
		return Settings{}, false
	}
	for _, x := range meta.Extensions {
		if x.Type != Type {
			continue
		}
		s, err := parseSettings(x.Config, nil, false)
		if err == nil && s.Namespace == source && s.Scope == string(scope.ID) {
			return s, true
		}
	}
	return Settings{}, false
}

func selfAuthored(a *tracker.Agent, w *domain.Work) bool {
	if a == nil {
		return false
	}
	if a.Work != "" {
		return a.Work == string(w.ID)
	}
	for _, k := range w.CorrelationKeys {
		if key, _, ok := tracker.ParseIssueKey(k.ExternalID); ok && strings.EqualFold(a.From, key) {
			return true
		}
	}
	return a.From == ""
}

// Match selects input for existing Work.
func (Route) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if !strings.HasPrefix(e.Type, Type+".") || e.Type == TypeDelivery {
		return extension.Skip("not a Bitbucket event about a pull request"), nil
	}
	if in.Work == nil {
		return extension.Skip("a pull request never starts Work; its issue's Work owns it"), nil
	}
	if e.CorrelationKey == nil {
		return extension.Skip("the event names no issue or pull request"), nil
	}
	if _, ok := settingsFor(in.Scope, e.Source); !ok {
		return extension.Skip("no bitbucket entry in this scope reads " + e.Source), nil
	}
	switch e.Type {
	case TypePullMerged, TypePullClosed, TypeChecksPassed, TypeChecksFailed:
		return extension.RouteDecision{Match: true, Reason: "news for the issue's agent"}, nil
	case TypeCommentHuman, TypeCommentAgent:
		var p CommentPayload
		if json.Unmarshal(e.Data.Data, &p) != nil {
			return extension.Skip("the comment data is unreadable"), nil
		}
		if p.Edited {
			return extension.Skip("an edited comment is not news"), nil
		}
		if e.Type == TypeCommentAgent && selfAuthored(p.Agent, in.Work) {
			return extension.Skip("the agent's own comment"), nil
		}
		return extension.RouteDecision{Match: true, Reason: "a comment for the issue's agent"}, nil
	}
	return extension.Skip(e.Type + " is not passed to the agent"), nil
}

// MaxDeliveryBytes bounds a webhook body.
const MaxDeliveryBytes = 10 << 20

// WebhookHandler serves one repository's webhook route: it verifies the
// signature before anything else, then hands the delivery to push, which
// must durably accept it before returning (202).
func WebhookHandler(s Settings, secret func(context.Context) (string, error), push func(context.Context, extension.Delivery) error, now func() time.Time) http.Handler {
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
		d, err := VerifyDelivery(s, key, r.Header, body, now())
		switch {
		case errors.Is(err, tracker.ErrSignature):
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
		w.WriteHeader(http.StatusAccepted)
	})
}
