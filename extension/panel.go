package extension

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Panels (issue #90). An action provider may also show panels on the scope
// and Work pages of operator views: a constrained document of sections,
// key/value fields, a table, links and buttons for its own actions. Core
// renders the document in every view (the web view and the TUI), so no
// extension code runs in the operator's browser and a panel holds no
// markup. A button names one of the provider's action IDs; the host shows it
// only while the provider lists that action on the same target, and invokes
// it through the action path, so a panel confers no authority of its own.

// Panel limits. A panel over them is not shown; core never truncates an
// extension's document into something it did not say.
const (
	MaxPanels        = 4
	MaxPanelSections = 16
	MaxPanelFields   = 32
	MaxPanelLinks    = 16
	MaxPanelColumns  = 8
	MaxPanelRows     = 100
	MaxPanelTitle    = 200
	MaxPanelValue    = 2048
	MaxPanelText     = 8192
	MaxPanelMessages = 200
	MaxPanelChoices  = 16
)

// Panel is one panel on a target. ID is stable ([a-z][a-z0-9_]*) and
// unique among the provider's panels on the target.
type Panel struct {
	ID       string
	Title    string
	Sections []PanelSection
}

// PanelSection is one block of a panel, shown in this order: its title,
// text, fields, table, messages, links and action buttons. Any may be
// empty, but not all of them.
type PanelSection struct {
	Title    string
	Text     string
	Fields   []PanelField
	Table    *PanelTable
	Messages []PanelMessage
	Links    []PanelLink
	Actions  []string
}

// Who wrote a panel message.
const (
	MessageOperator = "operator"
	MessageAgent    = "agent"
	MessageNote     = "note"
)

// PanelMessage is one line of a conversation (issue #242): what the
// operator or the agent said, or a note of something that happened. An
// agent's text is Markdown; the others are plain. Label, when set, reads
// in place of who wrote it ("Assistant asks"). Choices are the answers a
// question offered, and Chosen the one the operator gave.
type PanelMessage struct {
	From    string
	Label   string
	Text    string
	At      time.Time
	Choices []string
	Chosen  string
}

// PanelField is a key/value pair.
type PanelField struct {
	Name, Value string
}

// PanelTable is a table of text cells; every row has one cell per column.
type PanelTable struct {
	Columns []string
	Rows    [][]string
}

// PanelLink is a link out of the operator view: an http or https URL.
type PanelLink struct {
	Label, URL string
}

// PanelProvider is an ActionProvider that also shows panels. Panels
// returns what to show on the target now: nothing where it has nothing to
// say. Like Actions it must not write state, and it is called on every
// view refresh.
type PanelProvider interface {
	Panels(ctx context.Context, t ActionTarget) ([]Panel, error)
}

var panelID = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Validate reports why the panel cannot be shown, or nil.
func (p Panel) Validate() error {
	if !panelID.MatchString(p.ID) {
		return fmt.Errorf("panel id %q is not [a-z][a-z0-9_]*", p.ID)
	}
	if err := text("title", p.Title, MaxPanelTitle, true); err != nil {
		return err
	}
	if len(p.Sections) == 0 || len(p.Sections) > MaxPanelSections {
		return fmt.Errorf("panel %s has %d sections, want 1 to %d", p.ID, len(p.Sections), MaxPanelSections)
	}
	for i, s := range p.Sections {
		if err := s.validate(); err != nil {
			return fmt.Errorf("panel %s section %d: %w", p.ID, i+1, err)
		}
	}
	return nil
}

func (s PanelSection) validate() error {
	if s.Title == "" && s.Text == "" && len(s.Fields) == 0 && s.Table == nil && len(s.Messages) == 0 && len(s.Links) == 0 && len(s.Actions) == 0 {
		return fmt.Errorf("empty section")
	}
	if err := text("title", s.Title, MaxPanelTitle, false); err != nil {
		return err
	}
	if err := text("text", s.Text, MaxPanelText, false); err != nil {
		return err
	}
	if len(s.Fields) > MaxPanelFields {
		return fmt.Errorf("more than %d fields", MaxPanelFields)
	}
	for _, f := range s.Fields {
		if err := text("field name", f.Name, MaxPanelTitle, true); err != nil {
			return err
		}
		if err := text("field value", f.Value, MaxPanelValue, false); err != nil {
			return err
		}
	}
	if t := s.Table; t != nil {
		if len(t.Columns) == 0 || len(t.Columns) > MaxPanelColumns || len(t.Rows) > MaxPanelRows {
			return fmt.Errorf("table must have 1 to %d columns and at most %d rows", MaxPanelColumns, MaxPanelRows)
		}
		for _, c := range t.Columns {
			if err := text("column", c, MaxPanelTitle, false); err != nil {
				return err
			}
		}
		for _, r := range t.Rows {
			if len(r) != len(t.Columns) {
				return fmt.Errorf("table row has %d cells for %d columns", len(r), len(t.Columns))
			}
			for _, c := range r {
				if err := text("cell", c, MaxPanelValue, false); err != nil {
					return err
				}
			}
		}
	}
	if len(s.Messages) > MaxPanelMessages {
		return fmt.Errorf("more than %d messages", MaxPanelMessages)
	}
	for _, m := range s.Messages {
		if err := m.validate(); err != nil {
			return err
		}
	}
	if len(s.Links) > MaxPanelLinks {
		return fmt.Errorf("more than %d links", MaxPanelLinks)
	}
	for _, l := range s.Links {
		if err := text("link label", l.Label, MaxPanelTitle, true); err != nil {
			return err
		}
		if !httpURL(l.URL, MaxPanelValue) {
			return fmt.Errorf("link %q is not an http or https URL", l.URL)
		}
	}
	seen := map[string]bool{}
	for _, a := range s.Actions {
		if !panelID.MatchString(a) || seen[a] {
			return fmt.Errorf("action %q is not an action id, or repeats", a)
		}
		seen[a] = true
	}
	return nil
}

func (m PanelMessage) validate() error {
	if m.From != MessageOperator && m.From != MessageAgent && m.From != MessageNote {
		return fmt.Errorf("message from %q is not operator, agent or note", m.From)
	}
	if err := line("message label", m.Label, MaxPanelTitle, false); err != nil {
		return err
	}
	if err := text("message", m.Text, MaxPanelText, true); err != nil {
		return err
	}
	if len(m.Choices) > MaxPanelChoices {
		return fmt.Errorf("message has more than %d choices", MaxPanelChoices)
	}
	for _, c := range m.Choices {
		if err := line("choice", c, MaxPanelTitle, true); err != nil {
			return err
		}
	}
	return line("chosen", m.Chosen, MaxPanelTitle, false)
}

// httpURL reports whether s is an absolute http or https URL of at most
// limit bytes.
func httpURL(s string, limit int) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && len(s) <= limit
}

// text checks one string of a panel: valid UTF-8 within limit, with no
// control characters but newline and tab.
func text(what, s string, limit int, required bool) error {
	switch {
	case required && strings.TrimSpace(s) == "":
		return fmt.Errorf("%s is required", what)
	case len(s) > limit:
		return fmt.Errorf("%s is longer than %d bytes", what, limit)
	case !utf8.ValidString(s):
		return fmt.Errorf("%s is not UTF-8", what)
	}
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			return fmt.Errorf("%s has a control character", what)
		}
	}
	return nil
}
