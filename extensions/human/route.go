package human

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// WorkKindRequest is the kind of Work the `human` route makes for a
// request: one per request, on the route's binding, whose first input is
// the request's TypeRequestOpened and whose later input is the rest of its
// Events. What the Work does with them (send an email, page someone, post
// in another app) is the binding's: the route only delivers them.
const WorkKindRequest = "human.request"

// Route is the `human` route handler (issue #758). It takes the request
// Events the extension announced (never one posted under another source)
// and gives each request its own Work: TypeRequestOpened starts it, and
// the request's later Events are its input. It takes no config.
type Route struct{}

var _ extension.RouteHandler = Route{}

func (Route) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if !slices.Contains(RequestTypes, e.Type) {
		return extension.Skip("not a request event"), nil
	}
	if e.Source != Type {
		return extension.Skip("not announced by the human extension"), nil
	}
	if e.CorrelationKey == nil {
		return extension.Skip("the event names no request"), nil
	}
	if _, ok := ParseRequestKey(*e.CorrelationKey); !ok {
		return extension.Skip("the key is not a request's"), nil
	}
	if ValidateRoute(in.Route.Config.Data) != nil {
		return extension.Skip("the route config is unreadable"), nil
	}
	if in.Work == nil {
		if e.Type != TypeRequestOpened {
			return extension.Skip("no Work holds this request"), nil
		}
		return extension.RouteDecision{Match: true, Kind: WorkKindRequest, Display: requestDisplay(e.Data), Reason: "a new request starts its Work"}, nil
	}
	if in.Work.Kind != WorkKindRequest {
		return extension.Skip("the request's key is owned by other Work"), nil
	}
	return extension.RouteDecision{Match: true, Reason: "news of the request for its Work"}, nil
}

// requestDisplay titles a request's Work with its label, when it reads.
func requestDisplay(data domain.Payload) *domain.WorkDisplay {
	var p RequestPayload
	if data.Schema != SchemaRequest || json.Unmarshal(data.Data, &p) != nil {
		return nil
	}
	title := strings.TrimSpace(p.Label)
	if len(title) > extension.MaxDisplayTitle {
		title = strings.ToValidUTF8(title[:extension.MaxDisplayTitle], "")
	}
	d := &domain.WorkDisplay{Title: title}
	if extension.ValidateDisplay(d) != nil {
		return nil
	}
	return d
}

// ValidateRoute checks a `human` route's config: it takes none, so only an
// empty object (or nothing) is accepted.
func ValidateRoute(raw json.RawMessage) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return errors.New("the human route's config must be an object")
	}
	if len(m) > 0 {
		return errors.New("the human route takes no config")
	}
	return nil
}
