package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/open-conduit/open-conduit/internal/cli"
)

// The full-screen config editor (06-01, v1's schema-driven forms editor):
// the config file as a tree, the documented fields of whatever is selected
// (connection, entry, route, binding or resource config, from the adapter
// catalog), and the same JSON pointer edits as `config set/unset`. Nothing
// is saved until the whole document validates offline against the daemon's
// registrations; secret_refs values are shown redacted and must be
// references. It renders data only: no extension or executor branches.

type editMode int

const (
	browseEdit editMode = iota
	valueEdit
	keyEdit
	newValueEdit
)

// EditModel is the editor's bubbletea model (exported so tests drive it).
type EditModel struct {
	req     cli.EditRequest
	entries []cli.DocEntry
	cursor  int
	mode    editMode
	input   string
	target  string // pointer being edited or the parent being added to
	newKey  string
	dirty   bool
	saved   bool
	quit    bool
	confirm bool
	notice  string
	errs    []string
	width   int
	height  int
}

// NewEditor starts an editor on a request.
func NewEditor(req cli.EditRequest) EditModel {
	m := EditModel{req: req, width: 100, height: 30}
	m.entries = req.Doc.Entries()
	return m
}

// Saved reports whether the editor saved the document.
func (m EditModel) Saved() bool { return m.saved }

func (m EditModel) Init() tea.Cmd { return nil }

func (m EditModel) selected() (cli.DocEntry, bool) {
	if m.cursor < 0 || m.cursor >= len(m.entries) {
		return cli.DocEntry{}, false
	}
	return m.entries[m.cursor], true
}

func compact(s string) string {
	var b bytes.Buffer
	if json.Compact(&b, []byte(s)) == nil {
		return b.String()
	}
	return s
}

func (m *EditModel) refresh(keep string) {
	m.entries = m.req.Doc.Entries()
	m.cursor = min(m.cursor, max(len(m.entries)-1, 0))
	for i, e := range m.entries {
		if e.Pointer == keep {
			m.cursor = i
		}
	}
}

func (m EditModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		if m.mode != browseEdit {
			return m.typed(msg)
		}
		return m.browse(msg.String())
	}
	return m, nil
}

func (m EditModel) typed(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.mode, m.input, m.notice = browseEdit, "", "cancelled"
		return m, nil
	case tea.KeyBackspace:
		if r := []rune(m.input); len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
		return m, nil
	case tea.KeySpace:
		m.input += " "
		return m, nil
	case tea.KeyRunes:
		m.input += string(msg.Runes)
		return m, nil
	case tea.KeyEnter:
	default:
		return m, nil
	}
	m.errs = nil
	switch m.mode {
	case valueEdit:
		if err := m.req.Doc.Set(m.target, strings.TrimSpace(m.input)); err != nil {
			m.errs = []string{err.Error()}
			return m, nil
		}
		m.dirty, m.mode, m.notice = true, browseEdit, "set "+m.target
		m.refresh(m.target)
	case keyEdit:
		key := strings.TrimSpace(m.input)
		if key == "" || strings.ContainsAny(key, "/~") {
			m.errs = []string{"a field name is nonempty and has no / or ~"}
			return m, nil
		}
		m.newKey, m.mode, m.input = key, newValueEdit, ""
	case newValueEdit:
		ptr := m.target + "/" + m.newKey
		if err := m.req.Doc.Set(ptr, strings.TrimSpace(m.input)); err != nil {
			m.errs = []string{err.Error()}
			return m, nil
		}
		m.dirty, m.mode, m.notice = true, browseEdit, "added "+ptr
		m.refresh(ptr)
	}
	m.input = ""
	return m, nil
}

func (m EditModel) browse(k string) (tea.Model, tea.Cmd) {
	if k != "q" && k != "esc" {
		m.confirm = false
	}
	m.errs = nil
	switch k {
	case "ctrl+c":
		m.quit = true
		return m, tea.Quit
	case "q", "esc":
		if m.dirty && !m.confirm {
			m.confirm, m.notice = true, "unsaved changes: press q again to discard them, or s to save"
			return m, nil
		}
		m.quit = true
		return m, tea.Quit
	case "up", "k":
		m.cursor = max(m.cursor-1, 0)
	case "down", "j":
		m.cursor = min(m.cursor+1, max(len(m.entries)-1, 0))
	case "pgup":
		m.cursor = max(m.cursor-m.listHeight(), 0)
	case "pgdown", " ":
		m.cursor = min(m.cursor+m.listHeight(), max(len(m.entries)-1, 0))
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		m.cursor = max(len(m.entries)-1, 0)
	case "enter", "e":
		e, ok := m.selected()
		if !ok {
			return m, nil
		}
		v, err := m.req.Doc.Get(e.Pointer)
		if err != nil {
			m.errs = []string{err.Error()}
			return m, nil
		}
		if e.Secret {
			v = ""
		}
		m.mode, m.target, m.input = valueEdit, e.Pointer, compact(v)
	case "a":
		e, ok := m.selected()
		if !ok {
			return m, nil
		}
		target := e.Pointer
		kind := e.Kind
		if kind == "value" {
			// Add beside the selected value, in its parent.
			target = target[:strings.LastIndex(target, "/")]
			kind = "object"
			for _, p := range m.entries {
				if p.Pointer == target {
					kind = p.Kind
				}
			}
		}
		m.target = target
		if kind == "array" {
			m.newKey, m.mode, m.input = "-", newValueEdit, ""
		} else {
			m.mode, m.input = keyEdit, ""
		}
	case "d", "delete":
		e, ok := m.selected()
		if !ok {
			return m, nil
		}
		if err := m.req.Doc.Unset(e.Pointer); err != nil {
			m.errs = []string{err.Error()}
			return m, nil
		}
		m.dirty, m.notice = true, "removed "+e.Pointer
		m.refresh("")
	case "s":
		data := m.req.Doc.Bytes()
		details, err := m.req.Validate(data)
		if err != nil {
			m.errs = []string{"could not validate: " + err.Error()}
			return m, nil
		}
		if len(details) > 0 {
			for _, d := range details {
				m.errs = append(m.errs, fmt.Sprintf("%s  %s  %s", d.Path, d.Code, d.Message))
			}
			m.notice = "not saved: the config does not validate"
			return m, nil
		}
		if err := m.req.Save(data); err != nil {
			m.errs = []string{"could not save the config"}
			return m, nil
		}
		m.saved, m.dirty, m.notice = true, false, "saved "+m.req.Path+" (a running daemon changes only with config apply)"
	}
	return m, nil
}

func (m EditModel) listHeight() int { return max(m.height-14, 5) }

func clipTo(s string, n int) string {
	r := []rune(s)
	if n > 1 && len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// schemaLines describes the documented fields of the selected node's
// owner, marking which the document already has.
func (m EditModel) schemaLines(e cli.DocEntry) []string {
	title, fields := m.req.Doc.Schema(e.Pointer, m.req.Catalog)
	if title == "" {
		return []string{dimS.Render("no documented fields here (see open-conduit adapters)")}
	}
	// The object the fields belong to: the selected config/secret_refs node
	// or its ancestor.
	base := e.Pointer
	for _, marker := range []string{"/config", "/secret_refs", "/resource"} {
		if i := strings.LastIndex(base, marker); i >= 0 {
			base = base[:i+len(marker)]
			break
		}
	}
	present := map[string]bool{}
	for _, x := range m.entries {
		if strings.HasPrefix(x.Pointer, base+"/") && !strings.Contains(strings.TrimPrefix(x.Pointer, base+"/"), "/") {
			present[x.Label] = true
		}
	}
	out := []string{boldS.Render(title)}
	for _, f := range fields {
		mark := "  "
		if present[f.Name] {
			mark = "✓ "
		}
		line := mark + f.Name
		var notes []string
		if f.Required {
			notes = append(notes, "required")
		}
		if f.Default != "" {
			notes = append(notes, "default "+f.Default)
		}
		if len(f.Options) > 0 {
			notes = append(notes, strings.Join(f.Options, "|"))
		}
		if len(notes) > 0 {
			line += " (" + strings.Join(notes, "; ") + ")"
		}
		if f.Help != "" {
			line += " — " + f.Help
		}
		out = append(out, clipTo(line, max(m.width, 40)))
	}
	return out
}

func (m EditModel) View() string {
	var b strings.Builder
	state := ""
	if m.dirty {
		state = "  [modified]"
	}
	b.WriteString(boldS.Render("Open Conduit config "+m.req.Path) + dimS.Render(state) + "\n")
	b.WriteString(dimS.Render("↑/↓ move · enter/e edit · a add · d delete · s validate and save · q quit") + "\n")
	visible := m.listHeight()
	start := 0
	if m.cursor >= visible {
		start = m.cursor - visible + 1
	}
	for i := start; i < len(m.entries) && i < start+visible; i++ {
		e := m.entries[i]
		line := strings.Repeat("  ", e.Depth) + e.Label + ": " + e.Value
		line = clipTo(line, max(m.width, 20))
		if i == m.cursor {
			line = selS.Render(line)
		}
		b.WriteString(line + "\n")
	}
	if e, ok := m.selected(); ok {
		b.WriteString("\n" + dimS.Render(e.Pointer) + "\n")
		lines := m.schemaLines(e)
		for i, l := range lines {
			if i >= 8 {
				b.WriteString(dimS.Render(fmt.Sprintf("  … %d more (open-conduit adapters)", len(lines)-i)) + "\n")
				break
			}
			b.WriteString(l + "\n")
		}
	}
	switch m.mode {
	case valueEdit:
		b.WriteString(boldS.Render("value for "+m.target+" (JSON): "+m.input+"▏") + dimS.Render("  enter set · esc cancel") + "\n")
	case keyEdit:
		b.WriteString(boldS.Render("new field in "+m.target+": "+m.input+"▏") + dimS.Render("  enter next · esc cancel") + "\n")
	case newValueEdit:
		b.WriteString(boldS.Render("value for "+m.target+"/"+m.newKey+" (JSON): "+m.input+"▏") + dimS.Render("  enter add · esc cancel") + "\n")
	default:
		if m.notice != "" {
			b.WriteString(dimS.Render(m.notice) + "\n")
		}
	}
	for _, e := range m.errs {
		b.WriteString(errS.Render(e) + "\n")
	}
	return b.String()
}

// EditConfig runs the editor on the terminal and reports whether it saved.
func EditConfig(req cli.EditRequest, in io.Reader, out io.Writer) (bool, error) {
	p := tea.NewProgram(NewEditor(req), tea.WithInput(in), tea.WithOutput(out), tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return false, err
	}
	m, _ := final.(EditModel)
	return m.Saved(), nil
}
