package conformance_test

import (
	"context"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// asker is a reference panel provider over approver: a panel on open Work
// with its "approve" button. Each weakening drops one guarantee.
type asker struct {
	approver
	showAlways, unlisted, script, duplicate, noisy bool
	calls                                          *int
}

func (a asker) Panels(_ context.Context, t extension.ActionTarget) ([]extension.Panel, error) {
	if !a.showAlways && (t.Work == nil || t.Work.State.Terminal()) {
		return nil, nil
	}
	p := extension.Panel{ID: "approval", Title: "Approval", Sections: []extension.PanelSection{
		{Text: "Ship it?", Fields: []extension.PanelField{{Name: "for", Value: "operator"}}},
		{Table: &extension.PanelTable{Columns: []string{"#", "Option"}, Rows: [][]string{{"1", "yes"}}}, Links: []extension.PanelLink{{Label: "PR", URL: "https://example.com/pr/1"}}, Actions: []string{"approve"}},
	}}
	switch {
	case a.unlisted:
		p.Sections[1].Actions = []string{"merge"}
	case a.script:
		p.Sections[1].Links[0].URL = "javascript:alert(1)"
	case a.noisy:
		*a.calls++
		p.Title += string(rune('0' + *a.calls))
	}
	if a.duplicate {
		return []extension.Panel{p, p}, nil
	}
	return []extension.Panel{p}, nil
}

func askerFixture(p asker) conformance.PanelFixture {
	open := domain.Work{ID: "w", State: domain.WorkWaiting}
	done := domain.Work{ID: "w", State: domain.WorkCancelled}
	return conformance.PanelFixture{Provider: p, Shown: extension.ActionTarget{Work: &open}, Hidden: extension.ActionTarget{Work: &done}}
}

func TestReferencePanels(t *testing.T) {
	conformance.RunPanels(t, func() conformance.PanelFixture { return askerFixture(asker{}) })
}

func TestWeakenedPanelProvidersAreRejected(t *testing.T) {
	for name, p := range map[string]asker{
		"shows where nothing is outstanding": {showAlways: true},
		"offers an unlisted action":          {unlisted: true},
		"links outside http":                 {script: true},
		"repeats a panel":                    {duplicate: true},
		"is not deterministic":               {noisy: true, calls: new(int)},
	} {
		if err := conformance.CheckPanels(askerFixture(p)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
