package extension_test

import (
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

func TestPanelValidate(t *testing.T) {
	ok := func() extension.Panel {
		return extension.Panel{ID: "question", Title: "The agent asks", Sections: []extension.PanelSection{
			{Text: "Ship it?\n\tTonight.", Fields: []extension.PanelField{{Name: "for", Value: ""}}},
			{Table: &extension.PanelTable{Columns: []string{"#", "Option"}, Rows: [][]string{{"1", "yes"}}}, Links: []extension.PanelLink{{Label: "PR", URL: "https://example.com/1"}}, Actions: []string{"answer"}},
		}}
	}
	if err := ok().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, spoil := range map[string]func(*extension.Panel){
		"bad id":          func(p *extension.Panel) { p.ID = "Question" },
		"no title":        func(p *extension.Panel) { p.Title = " " },
		"no sections":     func(p *extension.Panel) { p.Sections = nil },
		"empty section":   func(p *extension.Panel) { p.Sections = append(p.Sections, extension.PanelSection{}) },
		"control char":    func(p *extension.Panel) { p.Sections[0].Text = "\x1b[31mred" },
		"long text":       func(p *extension.Panel) { p.Sections[0].Text = strings.Repeat("x", extension.MaxPanelText+1) },
		"ragged row":      func(p *extension.Panel) { p.Sections[1].Table.Rows[0] = []string{"1"} },
		"no columns":      func(p *extension.Panel) { p.Sections[1].Table.Columns = nil },
		"script link":     func(p *extension.Panel) { p.Sections[1].Links[0].URL = "javascript:alert(1)" },
		"relative link":   func(p *extension.Panel) { p.Sections[1].Links[0].URL = "/run?id=x" },
		"repeated action": func(p *extension.Panel) { p.Sections[1].Actions = []string{"answer", "answer"} },
		"bad action":      func(p *extension.Panel) { p.Sections[1].Actions = []string{"Answer!"} },
	} {
		p := ok()
		spoil(&p)
		if p.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
