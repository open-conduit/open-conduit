package runtime_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// Work an operator starts records what it was asked, its kind and context,
// on its core.work.created Event, as child Work does, so its first turn and
// the Event history show the task. Work started with no context records none.
func TestManualWorkCreationCarriesKindAndContext(t *testing.T) {
	f, _ := schedulerFixture(t, "reopen", routingOptions(routingHandlerFunc(func(extension.RouteInput) (extension.RouteDecision, error) {
		return extension.Skip("not this route's"), nil
	}), "queue"))
	driver := f.Driver.(*schedulerDriver)
	control, err := runtime.NewControl(f.Store, f.Clock.Now, driver.Router, driver.Scheduler, runtime.CausalLimits{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	asked := domain.Payload{Schema: "fixture.brief.v1", Data: []byte(`{"task":"summarise the week"}`)}
	_, created, err := control.CreateWork(ctx, runtime.ManualWork{ScopeID: "routing-scope", Kind: "fixture.manual", Binding: "fixture", DeliveryID: "briefed", Context: asked})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		WorkID, ParentID domain.ID
		Kind             string
		Context          *domain.Payload
	}
	if err = json.Unmarshal(created.Event.Data.Data, &got); err != nil {
		t.Fatal(err)
	}
	if created.Event.Source != runtime.ManualSource || got.Kind != "fixture.manual" || got.ParentID != "" || got.Context == nil || got.Context.Schema != asked.Schema || string(got.Context.Data) != string(asked.Data) {
		t.Fatalf("the creation does not carry what the operator asked: %s %s", created.Event.Source, created.Event.Data.Data)
	}

	_, created, err = control.CreateWork(ctx, runtime.ManualWork{ScopeID: "routing-scope", Kind: "fixture.manual", Binding: "fixture", DeliveryID: "unbriefed"})
	if err != nil {
		t.Fatal(err)
	}
	got.Context = nil
	if err = json.Unmarshal(created.Event.Data.Data, &got); err != nil || got.Kind != "fixture.manual" || got.Context != nil {
		t.Fatalf("Work with no context records one: %s %v", created.Event.Data.Data, err)
	}
}
