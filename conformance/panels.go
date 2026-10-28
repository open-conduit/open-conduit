package conformance

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

// PanelFixture drives an extension.PanelProvider through the panel contract
// (issue #90). Provider shows at least one panel on Shown and none on
// Hidden (nothing to say there). Provider is also the action provider its
// panels' buttons name.
type PanelFixture struct {
	Provider interface {
		extension.ActionProvider
		extension.PanelProvider
	}
	Shown  extension.ActionTarget
	Hidden extension.ActionTarget
}

// CheckPanels verifies, for a panel provider:
//   - on a target it has something to say about, it shows panels that are
//     each within the document limits (extension.Panel.Validate) with
//     unique IDs, no more than extension.MaxPanels, so the host renders
//     every one of them;
//   - every button names an action the provider lists on the same target,
//     so no button is dropped or offers what the action path refuses;
//   - showing is deterministic: the same target shows the same panels;
//   - on a target it has nothing to say about, it shows none.
func CheckPanels(f PanelFixture) error {
	ctx := context.Background()
	panels, err := f.Provider.Panels(ctx, f.Shown)
	if err != nil {
		return fmt.Errorf("panels: %v", err)
	}
	if len(panels) == 0 || len(panels) > extension.MaxPanels {
		return fmt.Errorf("shows %d panels, want 1 to %d", len(panels), extension.MaxPanels)
	}
	actions, err := f.Provider.Actions(ctx, f.Shown)
	if err != nil {
		return fmt.Errorf("actions: %v", err)
	}
	listed := map[string]bool{}
	for _, a := range actions {
		listed[a.ID] = true
	}
	seen := map[string]bool{}
	for _, p := range panels {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("panel not shown: %v", err)
		}
		if seen[p.ID] {
			return fmt.Errorf("duplicate panel %q", p.ID)
		}
		seen[p.ID] = true
		for _, s := range p.Sections {
			for _, a := range s.Actions {
				if !listed[a] {
					return fmt.Errorf("panel %s offers action %q the provider does not list here", p.ID, a)
				}
			}
		}
	}
	again, err := f.Provider.Panels(ctx, f.Shown)
	if err != nil || !reflect.DeepEqual(panels, again) {
		return fmt.Errorf("panels are not deterministic: %v", err)
	}
	hidden, err := f.Provider.Panels(ctx, f.Hidden)
	if err != nil || len(hidden) != 0 {
		return fmt.Errorf("panels shown where there is nothing to show: %d %v", len(hidden), err)
	}
	return nil
}

func RunPanels(t *testing.T, factory func() PanelFixture) {
	t.Helper()
	if err := CheckPanels(factory()); err != nil {
		t.Fatal(err)
	}
}
