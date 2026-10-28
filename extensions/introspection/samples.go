package introspection

import (
	"encoding/json"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Sample Events for a route trace ("Suppose…"): one of each family the
// handler can take, so a route shows the one it takes as a match and the
// others as skips. Nothing is offered for a scope without an `introspection`
// entry, which the route matches nothing in.

var _ extension.RouteSampler = Route{}

func sampleEvent(typ string, key domain.CorrelationKey, schema string, payload any) domain.Event {
	data, _ := json.Marshal(payload)
	return domain.Event{Type: typ, Source: key.Source, CorrelationKey: &key, Data: domain.Payload{Schema: schema, Data: data}}
}

// Samples offers a new report, a daily sweep and triage coming due, and a
// requested fix.
func (Route) Samples(scope domain.Scope, _ domain.Route) []extension.RouteSample {
	if !Enabled(scope) {
		return nil
	}
	now := time.Now().UTC().Truncate(time.Second)
	epoch := now.Format("2006-01-02")
	report := ReportKey(scope.ID, "example")
	fix := FixKey(scope.ID, "example")
	return []extension.RouteSample{
		{ID: "report-filed", Title: "An agent reports a defect", Event: sampleEvent(TypeReported, report, SchemaReport,
			Report{ID: "example", Scope: string(scope.ID), Kind: "docs", Title: "The setup guide names a command that no longer exists", Body: "The guide says to run the old command.", Key: report, At: now})},
		{ID: "sweep-due", Title: "The daily sweep comes due", Event: sampleEvent(TypeSweepDue, RunKey(scope.ID, WorkSweep, epoch), SchemaDue,
			Due{Run: WorkSweep, Slot: epoch, At: now, Epoch: epoch})},
		{ID: "triage-due", Title: "The daily triage comes due", Event: sampleEvent(TypeTriageDue, RunKey(scope.ID, WorkTriage, epoch), SchemaDue,
			Due{Run: WorkTriage, Slot: epoch, At: now, Epoch: epoch})},
		{ID: "fix-requested", Title: "A triage requests a fix", Event: sampleEvent(TypeFixRequested, fix, SchemaFix,
			Fix{ID: "example", Scope: string(scope.ID), Title: "Correct the setup guide", Body: "Name the command that exists."})},
	}
}
