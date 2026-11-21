package slack

import (
	"context"
	"errors"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Child requests (issue #681). A conversation hands tasks to child Work
// (create_work), and a child's gated call or question waits on a person
// the conversation's thread knows nothing of. With child_requests on, each
// request of a child, at any depth, shows in the thread of its nearest
// ancestor that is a conversation: as a task of the run's progress message
// (open, then complete once answered, or an error once refused or timed
// out), and, when a Slack person is among its responders, as a prompt
// with its buttons, which answers the child's request. Who may answer, and
// what an answer means, stay the provider's: Slack shows its requests'
// state (Host.Requests) and its actions (Host.Actions), and a press goes
// to Act as the person who pressed it. The conversation's own run is not
// involved and does not wake.

// RequestsFunc lists the requests reported on Work (inproc.Host.Requests).
type RequestsFunc func(ctx context.Context, scope, work domain.ID) ([]extension.RequestOffer, error)

// maxDepth bounds how far up a child's ancestors, or down a run's
// children, a pass looks.
const maxDepth = 16

// asker is a descendant Work that may hold requests, and the name its
// requests go by.
type asker struct {
	work domain.Work
	who  string
}

// latestBinding is the binding of w's latest attempt ("" before one).
func latestBinding(tx storage.Tx, w domain.ID) (string, error) {
	xs, err := tx.ListExecutions(w)
	if err != nil {
		return "", err
	}
	binding := ""
	var latest uint32
	for _, x := range xs {
		if x.Attempt >= latest {
			latest, binding = x.Attempt, x.ExecutorBinding
		}
	}
	return binding, nil
}

// who names the agent asking: its binding's Slack identity, else its
// binding, else its title, else its kind.
func (p *Projector) who(w domain.Work, binding string) string {
	if id, ok := p.o.Settings.IdentityOf(binding); ok && strings.TrimSpace(id.Username) != "" {
		return strings.TrimSpace(id.Username)
	}
	if binding != "" {
		return binding
	}
	if w.Display != nil && strings.TrimSpace(w.Display.Title) != "" {
		return strings.TrimSpace(w.Display.Title)
	}
	return w.Kind
}

// subtree is root and every Work below it, depth first, each with the name
// its requests go by. A descendant that is a conversation itself is left
// out with its own subtree: its requests are prompted in its own thread.
func (p *Projector) subtree(tx storage.Tx, root domain.Work, f *family) ([]asker, error) {
	var out []asker
	seen := map[domain.ID]bool{}
	var walk func(w domain.Work, depth int) error
	walk = func(w domain.Work, depth int) error {
		if seen[w.ID] || depth > maxDepth {
			return nil
		}
		seen[w.ID] = true
		if _, _, own := p.Thread(w); own {
			return nil
		}
		binding, err := latestBinding(tx, w.ID)
		if err != nil {
			return err
		}
		out = append(out, asker{work: w, who: p.who(w, binding)})
		kids, err := f.children(w.ID)
		if err != nil {
			return err
		}
		for _, k := range kids {
			if err := walk(k, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return out, walk(root, 0)
}

// childRequest is one request of a descendant.
type childRequest struct {
	asker
	extension.RequestOffer
	// names are the Slack names of the people it names, as the
	// workspace shows them.
	names []string
}

// requestsOf lists each asker's requests. A provider that fails now is
// asked again next pass.
func (p *Projector) requestsOf(ctx context.Context, as []asker) []childRequest {
	if p.o.Requests == nil || !p.o.Settings.ChildRequests {
		return nil
	}
	var out []childRequest
	for _, a := range as {
		rs, err := p.o.Requests(ctx, domain.ID(p.o.Settings.Scope), a.work.ID)
		if err != nil {
			p.hostRefused("requests", err)
			continue
		}
		for _, r := range rs {
			if strings.TrimSpace(r.Label) != "" {
				c := childRequest{asker: a, RequestOffer: r}
				if r.State == extension.RequestOpen {
					for _, u := range namedSlackPeople(r.Responders) {
						if name := p.o.Client.UserName(ctx, u); name != "" {
							c.names = append(c.names, name)
						}
					}
				}
				out = append(out, c)
			}
		}
	}
	return out
}

// title is how a request reads as a plan task, which is plain text: who
// asks, for what, and the Slack people it is for by name. Responders
// outside Slack, or that Slack can't name, are left out.
func (r childRequest) title() string {
	t := r.who + " asks: " + r.Label
	if w := r.Wording; w != nil && w.Wants != "" {
		t = r.who + " wants to " + strings.TrimSuffix(w.Wants, ".")
	} else if w != nil && w.Question != "" {
		t = r.who + " asks: " + w.Question
	}
	if len(r.names) > 0 {
		t += " (for " + strings.Join(r.names, ", ") + ")"
	}
	return t
}

// task is the request as a plan task: in progress while open, an error
// once refused or expired, else complete.
func (r childRequest) task() map[string]any {
	status := taskComplete
	switch r.State {
	case extension.RequestOpen:
		status = taskActive
	case extension.RequestRefused, extension.RequestExpired:
		status = taskError
	}
	return planTask("r"+digest(string(r.work.ID), r.Provider, r.ID)[:16], r.title(), status)
}

// slackAnswers reports whether a person on this extension's surface may
// answer: a responder of its kind, all of it or one person.
func slackAnswers(responders []string) bool {
	for _, r := range responders {
		if r == Type || strings.HasPrefix(r, Type+":") {
			return true
		}
	}
	return false
}

// childPrompts derives a prompt for each open request of child Work that a
// Slack person may answer, in the thread of the child's nearest ancestor
// that is a conversation, with the buttons of the action that answers it.
func (p *Projector) childPrompts(ctx context.Context) ([]Intent, error) {
	if !p.o.Settings.ChildRequests || p.o.Requests == nil || p.o.Actions == nil {
		return nil, nil
	}
	type held struct {
		asker
		channel, thread, binding string
	}
	var hs []held
	err := p.transact(ctx, func(tx storage.Tx) error {
		hs = nil
		works, err := tx.QueryWorks(storage.WorkQuery{ScopeID: domain.ID(p.o.Settings.Scope), States: askingStates})
		if err != nil {
			return err
		}
		f := newFamily(tx)
		for _, w := range works {
			if w.ParentID == "" {
				continue
			}
			if _, _, own := p.Thread(w); own {
				continue
			}
			anc, ok, err := p.conversationOf(w, f)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			channel, thread, _ := p.Thread(anc)
			binding, err := latestBinding(tx, w.ID)
			if err != nil {
				return err
			}
			ancBinding, err := latestBinding(tx, anc.ID)
			if err != nil {
				return err
			}
			hs = append(hs, held{asker{w, p.who(w, binding)}, channel, thread, ancBinding})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []Intent
	for _, h := range hs {
		rs := p.requestsOf(ctx, []asker{h.asker})
		// An action answers one request: when a provider names it for two,
		// which of them a press would answer is unknown, so neither is
		// prompted.
		named := map[string]int{}
		for _, r := range rs {
			if r.State == extension.RequestOpen && r.Action != "" {
				named[r.Provider+"/"+r.Action]++
			}
		}
		var offers []extension.ActionOffer
		for _, r := range rs {
			if r.State != extension.RequestOpen || r.Action == "" || named[r.Provider+"/"+r.Action] != 1 || !slackAnswers(r.Responders) {
				continue
			}
			if offers == nil {
				if offers, err = p.o.Actions(ctx, domain.ID(p.o.Settings.Scope), h.work.ID); err != nil {
					p.hostRefused("actions", err)
					break
				}
			}
			for _, o := range offers {
				if o.Provider != r.Provider || o.ID != r.Action {
					continue
				}
				req := r.Request
				in, ok := asked{who: h.who, work: h.work.ID, offer: o, request: &req, replies: !p.o.Settings.Presses()}.intent(h.channel, h.thread, h.binding)
				if !ok {
					continue
				}
				// Keyed by the request as well as what is shown, so each
				// request is prompted once.
				in.ID += "/" + digest(r.ID)[:8]
				out = append(out, in)
			}
		}
	}
	return out, nil
}

// conversationOf is w's nearest ancestor that is a conversation with a
// thread of this entry.
func (p *Projector) conversationOf(w domain.Work, f *family) (domain.Work, bool, error) {
	cur := w
	for range maxDepth {
		if cur.ParentID == "" {
			return domain.Work{}, false, nil
		}
		parent, ok, err := f.work(cur.ParentID)
		if err != nil || !ok {
			return domain.Work{}, false, err
		}
		if _, _, own := p.Thread(parent); own {
			return parent, true, nil
		}
		cur = parent
	}
	return domain.Work{}, false, nil
}

// askingStates are the states of Work that may be asking a person something:
// waiting on an answer, or ready but not started (held for an approval).
var askingStates = []domain.WorkState{domain.WorkWaiting, domain.WorkReady}

// family reads Work's parents and children as a pass needs them, each once
// a pass, so a pass reads the Work it follows rather than every Work ever
// made to find them.
type family struct {
	tx    storage.Tx
	works map[domain.ID]domain.Work
	gone  map[domain.ID]bool
	kids  map[domain.ID][]domain.Work
}

func newFamily(tx storage.Tx) *family {
	return &family{tx: tx, works: map[domain.ID]domain.Work{}, gone: map[domain.ID]bool{}, kids: map[domain.ID][]domain.Work{}}
}

// work is the Work id, if the extension can read it.
func (f *family) work(id domain.ID) (domain.Work, bool, error) {
	if w, ok := f.works[id]; ok {
		return w, true, nil
	}
	if f.gone[id] {
		return domain.Work{}, false, nil
	}
	w, err := f.tx.Work(id)
	if errors.Is(err, storage.ErrNotFound) {
		f.gone[id] = true
		return domain.Work{}, false, nil
	}
	if err != nil {
		return domain.Work{}, false, err
	}
	f.works[id] = w
	return w, true, nil
}

// children are id's child Work, oldest first.
func (f *family) children(id domain.ID) ([]domain.Work, error) {
	if kids, ok := f.kids[id]; ok {
		return kids, nil
	}
	kids, err := f.tx.QueryWorks(storage.WorkQuery{ParentID: id})
	if err != nil {
		return nil, err
	}
	f.kids[id] = kids
	return kids, nil
}
