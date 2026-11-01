package observe

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Route is the `observe` route handler. Its config names one source; a
// recorded item of a thread no Work owns starts observe.thread Work, and
// every later item of the thread is that Work's input. Which binding
// reported an item never matters here.
type Route struct{}

var (
	_ extension.RouteHandler = Route{}
	_ extension.RouteSampler = Route{}
)

func (Route) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if e.Type != TypeRecorded {
		return extension.Skip("not an observed item"), nil
	}
	rc, err := ParseRoute(in.Route.Config.Data)
	if err != nil {
		return extension.Skip("the route config is unreadable"), nil
	}
	if e.CorrelationKey == nil || e.CorrelationKey.Source != SourceNamespace(in.Scope.ID, rc.Source) {
		return extension.Skip("not an item from " + rc.Source), nil
	}
	var item Item
	if e.Data.Schema != SchemaItem || json.Unmarshal(e.Data.Data, &item) != nil {
		return extension.Skip("the item data is unreadable"), nil
	}
	if in.Work == nil {
		return extension.RouteDecision{Match: true, Kind: WorkKind, Reason: "a new thread in " + rc.Source, Display: display(item)}, nil
	}
	if in.Work.Kind != WorkKind {
		return extension.Skip("the thread is owned by other Work"), nil
	}
	return extension.RouteDecision{Match: true, Reason: "more of the thread"}, nil
}

// display titles a thread's Work with the first line of its first item.
func display(item Item) *domain.WorkDisplay {
	line, _, _ := strings.Cut(strings.TrimSpace(item.Text), "\n")
	title := extension.Clip(strings.Join(strings.Fields(line), " "), extension.MaxDisplayTitle)
	if title == "" {
		return nil
	}
	return &domain.WorkDisplay{Title: title, Ref: extension.Clip(item.Source, extension.MaxDisplayRef)}
}

// Samples offers an item starting a thread of the route's source.
func (Route) Samples(_ context.Context, in extension.SampleInput) []extension.RouteSample {
	rc, err := ParseRoute(in.Route.Config.Data)
	if err != nil {
		return nil
	}
	if _, ok := Sources(in.Scope)[rc.Source]; !ok {
		return nil
	}
	key := Key(in.Scope.ID, rc.Source, "1730451600.000100")
	data, _ := json.Marshal(Item{Source: rc.Source, Item: key.ExternalID, Thread: key.ExternalID, Text: "How do I reset my VPN password?"})
	return []extension.RouteSample{{ID: "item-recorded", Title: "A watcher records a new message in " + rc.Source,
		Event: domain.Event{Type: TypeRecorded, Source: key.Source, CorrelationKey: &key, OccurredAt: in.Now.UTC().Truncate(time.Second), Data: domain.Payload{Schema: SchemaItem, Data: data}}}}
}
