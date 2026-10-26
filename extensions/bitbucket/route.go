package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/extensions/tracker"
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
	if !strings.HasPrefix(e.Type, Type+".") || e.Type == TypeDelivery || in.Work == nil || e.CorrelationKey == nil {
		return extension.RouteDecision{}, nil
	}
	if _, ok := settingsFor(in.Scope, e.Source); !ok {
		return extension.RouteDecision{}, nil
	}
	switch e.Type {
	case TypePullMerged, TypePullClosed, TypeChecksPassed, TypeChecksFailed:
		return extension.RouteDecision{Match: true}, nil
	case TypeCommentHuman, TypeCommentAgent:
		var p CommentPayload
		if json.Unmarshal(e.Data.Data, &p) != nil || p.Edited {
			return extension.RouteDecision{}, nil
		}
		if e.Type == TypeCommentAgent && selfAuthored(p.Agent, in.Work) {
			return extension.RouteDecision{}, nil
		}
		return extension.RouteDecision{Match: true}, nil
	}
	return extension.RouteDecision{}, nil
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
