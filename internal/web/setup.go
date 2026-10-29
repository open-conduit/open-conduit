package web

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// quickStart is the docs page that sets Open Conduit up from a terminal.
const quickStart = "https://open-conduit.github.io/open-conduit/start/quick-start/"

// The setup extension's contract (extensions/setup): the entry type, the
// session's Work kind, the actions it offers and the Events a session is
// made of. Setup is the one page built for one extension, so these are the
// only names the view knows from it.
const (
	setupType          = "setup"
	setupKind          = "setup.session"
	setupStart         = "start"
	setupMessage       = "message"
	setupAnswer        = "answer"
	setupTypeMessage   = "setup.message"
	setupTypeQuestion  = "setup.question"
	setupTypeAnswer    = "setup.answer"
	setupRoleOperator  = "operator"
	setupPastSessions  = 5
	setupSessionEvents = "200"
)

type setupData struct {
	Config     client.Config
	Generation string
	QuickStart string
	// Scopes are the scopes with a setup entry. Start is the start action
	// one of them offers when no session is open, in StartScope.
	Scopes     []setupScope
	Start      *actionView
	StartScope string
	// Starting says a session was just started and its Work is not there
	// yet: the route creates it when it takes the start Event.
	Starting bool
	Session  *setupSession
	// Past are recently ended sessions.
	Past []workView
}

type setupScope struct {
	client.ConfigScope
	Entry string
}

// setupSession is an open session: its Work, the conversation so far and
// what the operator can send it.
type setupSession struct {
	W       workView
	Scope   string
	Model   string
	Started time.Time
	Msgs    []setupMsg
	// Message and Answer are the session's actions, when offered.
	Message, Answer *actionView
	// Changes is the session's change set, once the assistant proposes.
	Changes *setupChanges
	// Applied is the generation the session's changes last became, and
	// Doctor the daemon's checks read since.
	Applied string
	Doctor  *client.Doctor
}

// CheckMark is the mark for a doctor check's status.
func (*setupSession) CheckMark(status string) string { return configData{}.CheckMark(status) }

// setupMsg is one line of the conversation.
type setupMsg struct {
	Operator bool
	Text     string
	At       time.Time
	// Note is a change set Event drawn as a short line: a proposal, a
	// decision, a validation or an apply.
	Note bool
	// A question carries its context and choices; Open says it is the one
	// the session waits on, and Answer what the operator answered.
	Question bool
	Context  string
	Choices  []string
	Open     bool
	Answer   string
}

// Busy is what the session is doing while the operator waits for it, or "".
func (s *setupSession) Busy() string {
	switch s.W.Disp.Kind {
	case "running":
		return "The assistant is working"
	case "queued":
		return "Waiting for an agent slot"
	}
	return ""
}

// setupPage is Setup: a setup session in the browser. The session is Work
// in a scope with a setup entry; the page draws its Events as a
// conversation and sends the operator's messages and answers through the
// entry's actions. Without a setup scope it says how to set up from a
// terminal.
func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	p := &page{Title: "Setup", Nav: "setup"}
	d := setupData{QuickStart: quickStart}
	cfg, err := s.src.Config(ctx)
	if err != nil && !tooOld(err) {
		p.Err = errText(err)
	}
	d.Config, d.Generation = cfg, cfg.Generation
	for _, sc := range cfg.Scopes {
		for _, x := range sc.Extensions {
			if x.Type == setupType {
				d.Scopes = append(d.Scopes, setupScope{ConfigScope: sc, Entry: x.Name})
				break
			}
		}
	}
	if len(d.Scopes) > 0 {
		starting := r.URL.Query().Get("starting") != ""
		err := s.setupSessions(ctx, p, r.URL.Query().Get("session"), starting, &d)
		if d.Session == nil && starting {
			d.Start, d.Starting = nil, true
		}
		if err != nil && p.Err == "" {
			p.Err = errText(err)
		}
	}
	s.render(w, r, "setup", p, d)
}

// setupSessions finds the open session (or the one asked for), reads its
// conversation and actions, and otherwise the scope offering a new one.
// Fresh reads the Work again, as just after a start.
func (s *Server) setupSessions(ctx context.Context, p *page, want string, fresh bool, d *setupData) error {
	entries := map[string]string{}
	for _, sc := range d.Scopes {
		entries[sc.ID] = sc.Entry
	}
	var open *workView
	for _, v := range s.cachedWorld(ctx, fresh).Works {
		if v.Kind != setupKind || entries[v.ScopeID] == "" {
			continue
		}
		if v.ID == want || want == "" && v.Disp.Group != "closed" && (open == nil || v.Updated.After(open.Updated)) {
			open = &v
		}
		if v.Disp.Group == "closed" && v.ID != want {
			d.Past = append(d.Past, v)
		}
	}
	slices.SortFunc(d.Past, func(a, b workView) int { return b.Updated.Compare(a.Updated) })
	d.Past = d.Past[:min(len(d.Past), setupPastSessions)]
	if open != nil {
		return s.setupConversation(ctx, p, *open, entries[open.ScopeID], d)
	}
	for _, sc := range d.Scopes {
		actions, err := s.src.Actions(ctx, client.ActionTarget{ScopeID: sc.ID})
		if err != nil {
			if tooOld(err) {
				continue
			}
			return err
		}
		if a, ok := setupAction(actions, sc.Entry, setupStart); ok {
			d.Start, d.StartScope = &actionView{Action: a, Page: p, ScopeID: sc.ID, Back: "/setup?starting=1"}, sc.Name
			return nil
		}
	}
	return nil
}

func (s *Server) setupConversation(ctx context.Context, p *page, v workView, entry string, d *setupData) error {
	ss := &setupSession{W: v, Scope: v.ScopeID, Started: v.Created}
	for _, sc := range d.Scopes {
		if sc.ID == v.ScopeID && sc.Name != "" {
			ss.Scope = sc.Name
		}
	}
	if v.Context != nil {
		var c struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(v.Context.Data, &c)
		ss.Model = c.Model
	}
	d.Session = ss
	events, err := listAll(ctx, url.Values{"work_id": {v.ID}, "limit": {setupSessionEvents}}, s.src.Events)
	if err != nil {
		return err
	}
	ss.Msgs = setupMsgs(events)
	for _, e := range events {
		if e.Type == setupTypeApplied && e.Data != nil {
			var a struct{ Generation string }
			if json.Unmarshal(e.Data.Data, &a) == nil && a.Generation != "" {
				ss.Applied = a.Generation
			}
		}
	}
	if ss.Applied != "" {
		if doc, err := s.src.Doctor(ctx, false); err == nil {
			ss.Doctor = &doc
		}
	}
	if v.Disp.Group == "closed" {
		return nil
	}
	actions, err := s.src.Actions(ctx, client.ActionTarget{WorkID: v.ID})
	if err != nil && !tooOld(err) {
		return err
	}
	back := setupBack(v.ID)
	panels, err := s.src.Panels(ctx, client.ActionTarget{WorkID: v.ID})
	if err != nil && !tooOld(err) {
		return err
	}
	ss.Changes = setupChangeSet(panels, actions, entry, func(a client.Action) *actionView {
		return &actionView{Action: a, Page: p, WorkID: v.ID, Back: back}
	})
	if a, ok := setupAction(actions, entry, setupMessage); ok {
		ss.Message = &actionView{Action: a, Page: p, WorkID: v.ID, Back: back}
	}
	if a, ok := setupAction(actions, entry, setupAnswer); ok {
		ss.Answer = &actionView{Action: a, Page: p, WorkID: v.ID, Back: back}
		for i := len(ss.Msgs) - 1; i >= 0; i-- {
			if ss.Msgs[i].Question {
				ss.Msgs[i].Open = true
				break
			}
		}
	}
	return nil
}

// setupAction is the action id the setup entry offers, if it does.
func setupAction(actions []client.Action, entry, id string) (client.Action, bool) {
	i := slices.IndexFunc(actions, func(a client.Action) bool { return a.ID == id && providerName(a.Provider) == entry })
	if i < 0 {
		return client.Action{}, false
	}
	return actions[i], true
}

// setupMsgs is the conversation in a session's Events, in the order they
// were accepted. Events of other types are not part of it.
func setupMsgs(events []client.Event) []setupMsg {
	seq := func(e client.Event) uint64 { n, _ := strconv.ParseUint(e.Sequence, 10, 64); return n }
	slices.SortStableFunc(events, func(a, b client.Event) int { return cmp.Compare(seq(a), seq(b)) })
	var out []setupMsg
	asked := map[string]int{}
	for _, e := range events {
		if e.Data == nil {
			continue
		}
		at := parseTime(e.OccurredAt)
		switch e.Type {
		case setupTypeMessage:
			var m struct{ Role, Text string }
			if json.Unmarshal(e.Data.Data, &m) == nil && m.Text != "" {
				out = append(out, setupMsg{Operator: m.Role == setupRoleOperator, Text: m.Text, At: at})
			}
		case setupTypeQuestion:
			var q struct {
				ID      string   `json:"question_id"`
				Text    string   `json:"text"`
				Context string   `json:"context"`
				Choices []string `json:"choices"`
			}
			if json.Unmarshal(e.Data.Data, &q) == nil && q.Text != "" {
				asked[q.ID] = len(out)
				out = append(out, setupMsg{Question: true, Text: q.Text, Context: q.Context, Choices: q.Choices, At: at})
			}
		case setupTypeAnswer:
			var a struct {
				ID     string `json:"question_id"`
				Answer string `json:"answer"`
			}
			if json.Unmarshal(e.Data.Data, &a) != nil || a.Answer == "" {
				continue
			}
			if i, ok := asked[a.ID]; ok {
				out[i].Answer = a.Answer
			}
			out = append(out, setupMsg{Operator: true, Text: a.Answer, At: at})
		case setupTypeProposal, setupTypeDecision, setupTypeChecks, setupTypeApplied:
			if line, ok := setupNote(e); ok {
				out = append(out, setupMsg{Note: true, Text: line, At: at})
			}
		}
	}
	return out
}
