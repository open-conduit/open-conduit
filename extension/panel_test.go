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
			{Messages: []extension.PanelMessage{{From: extension.MessageParticipant, Label: "Agent asks", Text: "Ship it?\n\n**Tonight**", Choices: []string{"yes", "no"}, Chosen: "yes"}, {From: extension.MessageOperator, Text: "yes"}}},
		}}
	}
	if err := ok().Validate(); err != nil {
		t.Fatal(err)
	}
	// "agent" was MessageParticipant's value; a package may still send it.
	legacy := ok()
	legacy.Sections[2].Messages[0].From = "agent"
	if err := legacy.Validate(); err != nil {
		t.Fatalf("the old participant name: %v", err)
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
		"unknown author":  func(p *extension.Panel) { p.Sections[2].Messages[0].From = "system" },
		"empty message":   func(p *extension.Panel) { p.Sections[2].Messages[1].Text = " " },
		"two-line label":  func(p *extension.Panel) { p.Sections[2].Messages[0].Label = "a\nb" },
		"empty choice":    func(p *extension.Panel) { p.Sections[2].Messages[0].Choices = []string{""} },
		"many messages": func(p *extension.Panel) {
			p.Sections[2].Messages = make([]extension.PanelMessage, extension.MaxPanelMessages+1)
			for i := range p.Sections[2].Messages {
				p.Sections[2].Messages[i] = extension.PanelMessage{From: extension.MessageNote, Text: "x"}
			}
		},
	} {
		p := ok()
		spoil(&p)
		if p.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPageValidate(t *testing.T) {
	ok := func() extension.Page {
		return extension.Page{ID: "notes", Title: "Notes", Summary: "Take notes with an agent.", Entry: "notes", WorkKind: "notes.session", Start: "start", Compose: []string{"answer", "message"}, Route: "notes"}
	}
	if err := ok().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, spoil := range map[string]func(*extension.Page){
		"bad id":        func(p *extension.Page) { p.ID = "Notes" },
		"path id":       func(p *extension.Page) { p.ID = "a/b" },
		"no title":      func(p *extension.Page) { p.Title = "" },
		"no entry":      func(p *extension.Page) { p.Entry = "" },
		"no kind":       func(p *extension.Page) { p.WorkKind = "" },
		"no start":      func(p *extension.Page) { p.Start = "" },
		"bad compose":   func(p *extension.Page) { p.Compose = []string{"Send!"} },
		"many composes": func(p *extension.Page) { p.Compose = []string{"a", "b", "c", "d", "e"} },
	} {
		p := ok()
		spoil(&p)
		if p.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
