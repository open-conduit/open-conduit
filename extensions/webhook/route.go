package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Route is the `webhook` route handler. A delivery whose thread no Work
// owns opens webhook.delivery Work; a later delivery of the same thread
// (the same correlation value) is that Work's input. Once the Work has
// finished, later deliveries of its thread start nothing, as for any
// finished Work.
type Route struct{}

var _ extension.RouteHandler = Route{}

func (Route) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if e.Type != TypeDelivery {
		return extension.Skip("not a webhook delivery"), nil
	}
	if e.CorrelationKey == nil || e.CorrelationKey.Source != e.Source || !strings.HasPrefix(e.Source, Name+"/"+string(in.Scope.ID)+"/") {
		return extension.Skip("not a delivery to one of this scope's webhooks"), nil
	}
	rc, err := ParseRoute(in.Route.Config.Data)
	if err != nil {
		return extension.Skip("the route config is unreadable"), nil
	}
	p, ok := payload(e)
	if !ok {
		return extension.Skip("the delivery data is unreadable"), nil
	}
	if len(rc.Hooks) > 0 && !contains(rc.Hooks, p.Hook) {
		return extension.Skip("a hook this route doesn't take"), nil
	}
	if in.Work != nil {
		if in.Work.Kind != WorkKind {
			return extension.Skip("the thread is owned by other Work"), nil
		}
		return extension.RouteDecision{Match: true, Reason: "a later delivery of the thread"}, nil
	}
	return extension.RouteDecision{Match: true, CreateID: domain.ID(e.Source + "/" + e.CorrelationKey.ExternalID), Kind: WorkKind,
		Display: Display(p), Reason: "a new delivery to " + p.Hook}, nil
}

func payload(e domain.Event) (Payload, bool) {
	var p Payload
	if e.Data.Schema != SchemaDelivery || json.Unmarshal(e.Data.Data, &p) != nil || p.Hook == "" {
		return Payload{}, false
	}
	return p, true
}

// Display titles a delivery's Work with its hook and the correlation (or
// delivery) value.
func Display(p Payload) *domain.WorkDisplay {
	ref := p.Correlation
	if ref == "" {
		ref = p.Delivery
	}
	ref = strings.Join(strings.Fields(ref), " ")
	title := "A delivery to " + p.Hook
	if ref != "" {
		title += ": " + ref
	}
	return &domain.WorkDisplay{Title: extension.Clip(title, extension.MaxDisplayTitle), Ref: extension.Clip(ref, extension.MaxDisplayRef)}
}

// renderBody caps the bytes of a body a rendered delivery shows; the
// whole body stays on the Event, for webhook.delivery.read and a shell
// binding's OS_INPUT_FILE.
const renderBody = 8 << 10

// RenderEvent renders a delivery for an agent's turn: the hook, its keys
// and the body.
func RenderEvent(e domain.Event) (string, bool) {
	p, ok := payload(e)
	if !ok {
		return "", false
	}
	lines := []string{fmt.Sprintf("### A delivery to the %s webhook", p.Hook), "Event: " + string(e.ID)}
	if p.Delivery != "" {
		lines = append(lines, "Delivery: "+p.Delivery)
	}
	if p.Correlation != "" {
		lines = append(lines, "Correlation: "+p.Correlation)
	}
	if p.ContentType != "" {
		lines = append(lines, "Content type: "+p.ContentType)
	}
	out := strings.Join(lines, "\n")
	body := p.Text
	if body == "" && len(p.Body) > 0 {
		body = string(p.Body)
	}
	if body = strings.TrimSpace(body); body != "" {
		if len(body) > renderBody {
			n := renderBody
			for n > 0 && !utf8.RuneStart(body[n]) {
				n--
			}
			body = body[:n] + fmt.Sprintf("…\n\n(The body is %d bytes; this shows the first %d. Call %s with {\"event\": %q} for the whole body.)", len(body), n, OpRead, string(e.ID))
		}
		out += "\n\n" + body
	}
	return out, true
}
