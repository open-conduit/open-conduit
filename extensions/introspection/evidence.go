package introspection

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Evidence aggregation (05-03). A sweep folds the agents' corrections into
// the project's rules; a triage acts on their reports and problems. Both
// start from OpEvidence: the scope's open reports and the notes no earlier
// run handled, grouped by kind and leading line with counts, the Work they
// came from and sample bodies, plus the run's settings (v1's message lines).
// Notes are read from the configured namespaces through public storage (the
// GitHub tools write `github.notes`); the extension never writes another
// extension's namespace, so handling a note records it in HandledNamespace.

// HandledNamespace records the notes a sweep or triage handled.
const HandledNamespace = "introspection.notes"

// Note is an agent's note as the notes namespaces hold it (the GitHub tools'
// record shape: kind correction or problem, area the correction's area or
// the problem's kind).
type Note struct {
	// ID is "<namespace>:<key>", set when read.
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Area      string    `json:"area"`
	Scope     string    `json:"scope"`
	Work      string    `json:"work"`
	Execution string    `json:"execution"`
	Issue     int       `json:"issue,omitempty"`
	Body      string    `json:"body"`
	At        time.Time `json:"at"`
}

// Note kinds.
const (
	NoteCorrection = "correction"
	NoteProblem    = "problem"
)

func handledKey(scope domain.ID, id string) string { return "handled/" + string(scope) + "/" + id }

// Notes lists a scope's notes of kind from the namespaces, oldest first;
// handled ones are left out unless all is set.
func Notes(tx storage.Tx, namespaces []string, scope domain.ID, kind string, all bool) ([]Note, error) {
	handled := map[string]bool{}
	if !all {
		rows, err := tx.List(HandledNamespace, "handled/"+string(scope)+"/")
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			handled[strings.TrimPrefix(r.Key, "handled/"+string(scope)+"/")] = true
		}
	}
	var out []Note
	for _, ns := range namespaces {
		rows, err := tx.List(ns, kind+"/")
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			var n Note
			if json.Unmarshal(r.Value, &n) != nil || n.Kind != kind || n.Scope != string(scope) {
				continue
			}
			n.ID = ns + ":" + r.Key
			if !handled[n.ID] {
				out = append(out, n)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// noteIDs reports which of ids are the scope's notes in the namespaces.
func noteIDs(tx storage.Tx, cfg Config, scope domain.ID, ids []string) (map[string]bool, error) {
	known := map[string]bool{}
	if len(ids) == 0 {
		return known, nil
	}
	for _, kind := range []string{NoteCorrection, NoteProblem} {
		notes, err := Notes(tx, cfg.NoteNamespaces(), scope, kind, true)
		if err != nil {
			return nil, err
		}
		for _, n := range notes {
			known[n.ID] = true
		}
	}
	return known, nil
}

// Group is one aggregated cause candidate: items sharing a kind and a
// normalized leading line.
type Group struct {
	Kind  string `json:"kind"`
	Lead  string `json:"lead"`
	Count int    `json:"count"`
	// IDs are the report or note IDs, Works the distinct Work they came
	// from, Issues the issue numbers notes name.
	IDs     []string  `json:"ids"`
	Works   []string  `json:"works"`
	Issues  []int     `json:"issues,omitempty"`
	First   time.Time `json:"first"`
	Last    time.Time `json:"last"`
	Samples []string  `json:"samples"`
}

type item struct {
	id, kind, lead, work, body string
	issue                      int
	at                         time.Time
}

// lead is an item's normalized leading line, the grouping hint.
func lead(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.Join(strings.Fields(strings.ToLower(line)), " ")
	if len(line) > 120 {
		line = line[:120]
		for !utf8.ValidString(line) {
			line = line[:len(line)-1]
		}
	}
	return line
}

func groups(items []item) []Group {
	index := map[string]int{}
	var out []Group
	for _, it := range items {
		k := it.kind + "\x00" + it.lead
		i, ok := index[k]
		if !ok {
			i = len(out)
			index[k] = i
			out = append(out, Group{Kind: it.kind, Lead: it.lead, First: it.at, IDs: []string{}, Works: []string{}, Samples: []string{}})
		}
		g := &out[i]
		g.Count++
		g.IDs = append(g.IDs, it.id)
		if it.work != "" && !contains(g.Works, it.work) {
			g.Works = append(g.Works, it.work)
		}
		if it.issue > 0 && !containsInt(g.Issues, it.issue) {
			g.Issues = append(g.Issues, it.issue)
		}
		if it.at.After(g.Last) {
			g.Last = it.at
		}
		if len(g.Samples) < 2 && !contains(g.Samples, it.body) {
			body := it.body
			if len(body) > 1000 {
				body = body[:1000]
				for !utf8.ValidString(body) {
					body = body[:len(body)-1]
				}
			}
			g.Samples = append(g.Samples, body)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func containsInt(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// Settings is a run's message: v1's lines, from the current generation.
type Settings struct {
	Notes      []string `json:"notes"`
	Rules      []string `json:"rules,omitempty"`
	Guide      string   `json:"guide,omitempty"`
	Land       string   `json:"land"`
	Remote     string   `json:"remote"`
	FileIssues bool     `json:"file_issues"`
	Scheduled  string   `json:"scheduled,omitempty"`
}

func settings(cfg Config, run string) Settings {
	s := cfg.Sweep
	if run == WorkTriage {
		s = cfg.Triage
	}
	out := Settings{Notes: cfg.NoteNamespaces(), Land: LandPush, Remote: "origin"}
	if s == nil {
		return out
	}
	out.Rules, out.Guide, out.FileIssues = s.Rules, s.Guide, s.FileIssues
	if s.Land != "" {
		out.Land = s.Land
	}
	if s.Remote != "" {
		out.Remote = s.Remote
	}
	out.Scheduled = strings.TrimSpace(s.Cron + " " + s.TimeZone)
	return out
}

// Evidence is OpEvidence's output.
type Evidence struct {
	For      string   `json:"for"`
	Settings Settings `json:"settings"`
	// Reports (triage): open reports, grouped; Fixing lists reports a
	// requested fix already names.
	Reports *Section `json:"reports,omitempty"`
	Fixing  []string `json:"fixing,omitempty"`
	// Problems (triage) and Corrections (sweep): unhandled notes, grouped.
	Problems    *Section `json:"problems,omitempty"`
	Corrections *Section `json:"corrections,omitempty"`
}

// Section is one kind of evidence.
type Section struct {
	Count  int     `json:"count"`
	Groups []Group `json:"groups"`
}

// Aggregate builds a run's evidence in tx.
func Aggregate(tx storage.Tx, cfg Config, scope domain.ID, run string) (Evidence, error) {
	ev := Evidence{For: run, Settings: settings(cfg, run)}
	notesOf := func(kind string) (*Section, error) {
		notes, err := Notes(tx, cfg.NoteNamespaces(), scope, kind, false)
		if err != nil {
			return nil, err
		}
		items := make([]item, 0, len(notes))
		for _, n := range notes {
			items = append(items, item{id: n.ID, kind: n.Area, lead: lead(n.Body), work: n.Work, body: n.Body, issue: n.Issue, at: n.At})
		}
		return &Section{Count: len(notes), Groups: groups(items)}, nil
	}
	var err error
	if run == WorkSweep {
		ev.Corrections, err = notesOf(NoteCorrection)
		return ev, err
	}
	reports, err := Reports(tx, scope)
	if err != nil {
		return ev, err
	}
	var items []item
	for _, r := range reports {
		switch r.State {
		case StateOpen:
			items = append(items, item{id: r.ID, kind: r.Kind, lead: lead(r.Title), work: r.Work, body: r.Title + "\n\n" + r.Body, at: r.At})
		case StateFixing:
			ev.Fixing = append(ev.Fixing, r.ID)
		}
	}
	ev.Reports = &Section{Count: len(items), Groups: groups(items)}
	ev.Problems, err = notesOf(NoteProblem)
	return ev, err
}

func (t *Tools) evidence(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
	cfg, err := t.entry(ctx, a)
	if err != nil {
		return domain.Payload{}, err
	}
	var q struct {
		For string `json:"for"`
	}
	if err := strict(in.Data, &q); err != nil || q.For != WorkSweep && q.For != WorkTriage {
		return domain.Payload{}, refuse("invalid_input", "for must be sweep or triage")
	}
	var ev Evidence
	if err := t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		var err error
		ev, err = Aggregate(tx, cfg, a.ScopeID, q.For)
		return err
	}); err != nil {
		return domain.Payload{}, err
	}
	return output(ev)
}

// handledNote is a HandledNamespace record.
type handledNote struct {
	Summary   string    `json:"summary"`
	Work      string    `json:"work"`
	Execution string    `json:"execution"`
	At        time.Time `json:"at"`
}

var errNoNote = errors.New("no such note")

func (t *Tools) notesResolve(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
	cfg, err := t.entry(ctx, a)
	if err != nil {
		return domain.Payload{}, err
	}
	var r struct {
		Notes   []string `json:"notes"`
		Summary string   `json:"summary"`
	}
	if err := strict(in.Data, &r); err != nil {
		return domain.Payload{}, refuse("invalid_input", "the input is malformed or has unknown fields (notes, summary)")
	}
	r.Summary = strings.TrimSpace(r.Summary)
	if len(r.Notes) == 0 || len(r.Notes) > 500 || r.Summary == "" || len(r.Summary) > 2000 || !utf8.ValidString(r.Summary) {
		return domain.Payload{}, refuse("invalid_input", "1 to 500 notes and a summary of 1 to 2000 bytes are required")
	}
	var missing string
	handled := 0
	err = t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		handled = 0
		known, err := noteIDs(tx, cfg, a.ScopeID, r.Notes)
		if err != nil {
			return err
		}
		value, _ := json.Marshal(handledNote{Summary: r.Summary, Work: string(a.WorkID), Execution: string(a.ExecutionID), At: t.o.Now().UTC()})
		for _, id := range r.Notes {
			if !known[id] {
				missing = id
				return errNoNote
			}
			if _, err := tx.Get(HandledNamespace, handledKey(a.ScopeID, id)); err == nil {
				continue
			} else if !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			if err := tx.Put(HandledNamespace, handledKey(a.ScopeID, id), value); err != nil {
				return err
			}
			handled++
		}
		return nil
	})
	if errors.Is(err, errNoNote) {
		return domain.Payload{}, refuse("not_found", "this scope has no note %q", missing)
	}
	if err != nil {
		return domain.Payload{}, err
	}
	return output(map[string]any{"handled": handled})
}
