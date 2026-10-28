package schedule

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Sample Events for a route trace ("Suppose…"), one set per job of each
// schedule entry of the scope, written as the entry's triggers would produce
// them. A route that names jobs offers only those.

var _ extension.RouteSampler = Route{}

func sampleEvent(typ, source, job string, p TriggerPayload) domain.Event {
	p.Job = job
	data, _ := json.Marshal(p)
	return domain.Event{Type: typ, Source: source,
		CorrelationKey: &domain.CorrelationKey{Source: source, ExternalID: JobKey(job)},
		Data:           domain.Payload{Schema: SchemaTrigger, Data: data}}
}

// Samples offers, for each job, a scheduled run coming due (when the job has
// a timer), a POST to its webhook (when it takes one) and a run by hand.
func (Route) Samples(_ context.Context, in extension.SampleInput) []extension.RouteSample {
	scope, route := in.Scope, in.Route
	var only []string
	if len(route.Config.Data) > 0 {
		if rc, err := ParseRoute(route.Config.Data); err == nil {
			only = rc.Jobs
		}
	}
	var out []extension.RouteSample
	entries := Entries(scope)
	for _, s := range entries {
		for _, j := range s.Jobs {
			if len(only) > 0 && !slices.Contains(only, j.Name) {
				continue
			}
			id := func(kind string) string {
				if len(entries) > 1 {
					return s.Namespace + "/" + j.Name + "/" + kind
				}
				return j.Name + "/" + kind
			}
			if j.Recurring() {
				at := in.Now.UTC().Truncate(time.Minute)
				out = append(out, extension.RouteSample{ID: id("due"), Title: "A scheduled job comes due: " + j.Name,
					Event: sampleEvent(TypeSlot, s.Namespace, j.Name, TriggerPayload{Kind: KindTimer, Trigger: j.Describe(), Slot: at.Format("2006-01-02 15:04 UTC"), At: &at})})
			}
			if j.Webhook() {
				out = append(out, extension.RouteSample{ID: id("webhook"), Title: "Something POSTs to the webhook of " + j.Name,
					Event: sampleEvent(TypeWebhook, s.Namespace, j.Name, TriggerPayload{Kind: KindWebhook, Trigger: "webhook", ContentType: "application/json", Body: json.RawMessage(`{}`)})})
			}
			manual := sampleEvent("schedule.run", ManualSource, j.Name, TriggerPayload{Kind: KindManual, Trigger: "by hand"})
			// An operator's Event is addressed to the job's key in the entry's namespace.
			manual.CorrelationKey.Source = s.Namespace
			out = append(out, extension.RouteSample{ID: id("manual"), Title: "An operator runs " + j.Name + " by hand", Event: manual})
		}
	}
	return out
}
