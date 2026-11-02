package sentry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/storage"
)

// Operations.
const (
	OpView    = "sentry.issue.view"
	OpComment = "sentry.issue.comment"
	OpResolve = "sentry.issue.resolve"
)

// CallsNS records the comments already posted per request ID, so a retried
// call never posts twice.
const CallsNS = "sentry.calls"

// MaxFrames bounds the stack frames an exception's trace returns (the
// innermost ones).
const MaxFrames = 40

type tools struct{ st *state }

func refuse(code, format string, args ...any) error {
	return &extension.OperationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// refusal maps a service failure onto a safe refusal: never a token or a
// raw service body.
func refusal(err error) error {
	var op *extension.OperationError
	var api *APIError
	switch {
	case errors.As(err, &op), errors.Is(err, extension.ErrUnauthorized), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, ErrCredential):
		return refuse("sentry_credential", "the Sentry connection's token could not be used; report it as a problem")
	case errors.As(err, &api) && api.Status == 404:
		return refuse("not_found", "Sentry has no such issue in this organization")
	case errors.As(err, &api):
		return refuse("sentry_refused", "Sentry refused the request (HTTP %d)", api.Status)
	}
	return refuse("sentry_unavailable", "Sentry could not be reached or answered with an error; try again")
}

// bound is what a call serves: the calling Work, the entry it reads with,
// and the issue the Work was opened for ("" when it wasn't opened for one).
type bound struct {
	work  domain.Work
	entry *entry
	issue string
}

// bind resolves the calling Work from durable state. A Work opened for a
// Sentry issue binds to that issue and its entry; any other Work in a scope
// with exactly one Sentry entry binds to that entry with no issue of its
// own, so it may read issues but not change them.
func (t tools) bind(ctx context.Context, a extension.Authority) (*bound, error) {
	var w domain.Work
	if err := sdk.NewStore(sdk.HostFrom(ctx)).Transact(ctx, storage.Guard{Now: t.st.o.Now()}, func(tx storage.Tx) error {
		var err error
		w, err = tx.Work(a.WorkID)
		return err
	}); err != nil || w.ScopeID != a.ScopeID {
		return nil, extension.ErrUnauthorized
	}
	var inScope []*entry
	for _, e := range t.st.list() {
		if e.scope != string(w.ScopeID) {
			continue
		}
		inScope = append(inScope, e)
		for _, k := range w.CorrelationKeys {
			if k.Source == e.settings.Namespace && k.ExternalID != "" {
				return &bound{work: w, entry: e, issue: k.ExternalID}, nil
			}
		}
	}
	if len(inScope) == 1 {
		return &bound{work: w, entry: inScope[0]}, nil
	}
	return nil, refuse("no_sentry_entry", "this scope has no single Sentry entry to read with")
}

// own is the issue a changing call acts on: only the one the Work was
// opened for.
func (b *bound) own() (string, error) {
	if b.issue == "" {
		return "", refuse("not_sentry_work", "this Work was not opened for a Sentry issue, so it can't change one")
	}
	return b.issue, nil
}

// ViewInput is sentry.issue.view's input.
type ViewInput struct {
	// Issue is a Sentry issue ID; omit it for the Work's own issue.
	Issue string `json:"issue,omitempty"`
}

// Frame is one stack frame.
type Frame struct {
	File     string   `json:"file,omitempty"`
	Function string   `json:"function,omitempty"`
	Module   string   `json:"module,omitempty"`
	Line     int      `json:"line,omitempty"`
	Column   int      `json:"column,omitempty"`
	InApp    bool     `json:"in_app"`
	Context  []string `json:"context,omitempty"`
}

// Exception is one exception in the event, its frames outermost first.
type Exception struct {
	Type    string  `json:"type,omitempty"`
	Value   string  `json:"value,omitempty"`
	Module  string  `json:"module,omitempty"`
	Frames  []Frame `json:"frames,omitempty"`
	Omitted int     `json:"frames_omitted,omitempty"`
}

// Commit is a suspect commit Sentry links to the event.
type Commit struct {
	ID         string `json:"id"`
	Message    string `json:"message,omitempty"`
	Author     string `json:"author,omitempty"`
	Repository string `json:"repository,omitempty"`
	URL        string `json:"url,omitempty"`
}

// Event is the latest event of an issue.
type Event struct {
	ID          string            `json:"id"`
	Title       string            `json:"title,omitempty"`
	Message     string            `json:"message,omitempty"`
	Platform    string            `json:"platform,omitempty"`
	Date        string            `json:"date,omitempty"`
	Release     string            `json:"release,omitempty"`
	Environment string            `json:"environment,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	Exceptions  []Exception       `json:"exceptions,omitempty"`
}

// ViewOutput is sentry.issue.view's output.
type ViewOutput struct {
	IssuePayload
	Count      string   `json:"count,omitempty"`
	Users      int      `json:"users,omitempty"`
	LastSeen   string   `json:"last_seen,omitempty"`
	Own        bool     `json:"own"`
	Event      *Event   `json:"latest_event,omitempty"`
	Suspects   []Commit `json:"suspect_commits,omitempty"`
	Incomplete []string `json:"incomplete,omitempty"`
}

// BodyInput is sentry.issue.comment's input.
type BodyInput struct {
	Body string `json:"body"`
}

// Done is a changing operation's output.
type Done struct {
	Issue     string `json:"issue"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

func (t tools) register(ext *sdk.Extension) {
	ext.Operation(OpView, sdk.Handler(t.view), sdk.Guidance(
		"Read a Sentry issue: its state, how often it happens, its latest event with the stack trace (innermost frames, with source context for your own code), the release and environment, and the commits Sentry suspects. Omit `issue` for the issue this Work was opened for; give a Sentry issue ID to read another issue in the same organization (an incident's alert often names one). `incomplete` lists the parts Sentry couldn't give."))
	ext.Operation(OpComment, sdk.Handler(t.comment), sdk.Guidance(
		"Comment on the Sentry issue this Work was opened for, in Markdown: what you found, the suspect commit, a link to the issue or pull request you opened. Give a `request_id` so a retry never posts twice."))
	ext.Operation(OpResolve, sdk.Handler(t.resolve), sdk.Guidance(
		"Mark the Sentry issue this Work was opened for resolved, once the fix has shipped. Sentry reopens it as a regression if it happens again."))
}

func (t tools) view(ctx context.Context, a extension.Authority, in ViewInput) (ViewOutput, error) {
	b, err := t.bind(ctx, a)
	if err != nil {
		return ViewOutput{}, err
	}
	id := in.Issue
	if id == "" {
		if id, err = b.own(); err != nil {
			return ViewOutput{}, err
		}
	}
	if strings.ContainsAny(id, "/?#") {
		return ViewOutput{}, refuse("bad_request", "issue must be a Sentry issue ID")
	}
	c := b.entry.client
	i, err := c.Issue(ctx, id)
	if err != nil {
		return ViewOutput{}, refusal(err)
	}
	out := ViewOutput{IssuePayload: IssuePayload{Org: b.entry.settings.Org, Issue: i.ID, ShortID: i.ShortID, Title: i.Title, Culprit: i.Culprit, Level: i.Level,
		Status: i.Status, Substatus: i.Substatus, Project: i.Project.Slug, URL: i.Permalink, FirstSeen: i.FirstSeen},
		Count: i.Count, Users: i.UserCount, LastSeen: i.LastSeen, Own: i.ID == b.issue}
	raw, err := c.LatestEvent(ctx, id)
	if err != nil {
		out.Incomplete = append(out.Incomplete, "latest_event")
		return out, nil
	}
	ev := ParseEvent(raw)
	out.Event = &ev
	if ev.ID != "" && i.Project.Slug != "" {
		if raw, err := c.Committers(ctx, i.Project.Slug, ev.ID); err == nil {
			out.Suspects = ParseCommitters(raw)
		} else {
			out.Incomplete = append(out.Incomplete, "suspect_commits")
		}
	}
	return out, nil
}

// ParseEvent reads the parts of a Sentry event an agent needs.
func ParseEvent(raw json.RawMessage) Event {
	var e struct {
		ID          string `json:"id"`
		EventID     string `json:"eventID"`
		Title       string `json:"title"`
		Message     string `json:"message"`
		Platform    string `json:"platform"`
		DateCreated string `json:"dateCreated"`
		Tags        []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"tags"`
		Release *struct {
			Version string `json:"version"`
		} `json:"release"`
		Entries []struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		} `json:"entries"`
	}
	_ = json.Unmarshal(raw, &e)
	out := Event{ID: e.EventID, Title: e.Title, Message: e.Message, Platform: e.Platform, Date: e.DateCreated}
	if out.ID == "" {
		out.ID = e.ID
	}
	for _, t := range e.Tags {
		if out.Tags == nil {
			out.Tags = map[string]string{}
		}
		out.Tags[t.Key] = t.Value
	}
	out.Release, out.Environment = out.Tags["release"], out.Tags["environment"]
	if e.Release != nil && e.Release.Version != "" {
		out.Release = e.Release.Version
	}
	for _, en := range e.Entries {
		if en.Type != "exception" {
			continue
		}
		var data struct {
			Values []struct {
				Type       string `json:"type"`
				Value      string `json:"value"`
				Module     string `json:"module"`
				Stacktrace *struct {
					Frames []struct {
						Filename string  `json:"filename"`
						AbsPath  string  `json:"absPath"`
						Function string  `json:"function"`
						Module   string  `json:"module"`
						LineNo   int     `json:"lineNo"`
						ColNo    int     `json:"colNo"`
						InApp    bool    `json:"inApp"`
						Context  [][]any `json:"context"`
					} `json:"frames"`
				} `json:"stacktrace"`
			} `json:"values"`
		}
		_ = json.Unmarshal(en.Data, &data)
		for _, v := range data.Values {
			x := Exception{Type: v.Type, Value: v.Value, Module: v.Module}
			if v.Stacktrace != nil {
				frames := v.Stacktrace.Frames
				if len(frames) > MaxFrames {
					x.Omitted = len(frames) - MaxFrames
					frames = frames[len(frames)-MaxFrames:]
				}
				for _, f := range frames {
					file := f.Filename
					if file == "" {
						file = f.AbsPath
					}
					fr := Frame{File: file, Function: f.Function, Module: f.Module, Line: f.LineNo, Column: f.ColNo, InApp: f.InApp}
					if f.InApp {
						for _, c := range f.Context {
							if len(c) == 2 {
								fr.Context = append(fr.Context, fmt.Sprintf("%v: %v", c[0], c[1]))
							}
						}
					}
					x.Frames = append(x.Frames, fr)
				}
			}
			out.Exceptions = append(out.Exceptions, x)
		}
	}
	return out
}

// ParseCommitters reads Sentry's suspect committers answer.
func ParseCommitters(raw json.RawMessage) []Commit {
	var c struct {
		Committers []struct {
			Author struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"author"`
			Commits []struct {
				ID         string `json:"id"`
				Message    string `json:"message"`
				Repository struct {
					Name string `json:"name"`
					URL  string `json:"url"`
				} `json:"repository"`
			} `json:"commits"`
		} `json:"committers"`
	}
	_ = json.Unmarshal(raw, &c)
	var out []Commit
	for _, who := range c.Committers {
		author := who.Author.Name
		if author == "" {
			author = who.Author.Email
		}
		for _, cm := range who.Commits {
			x := Commit{ID: cm.ID, Message: cm.Message, Author: author, Repository: cm.Repository.Name}
			if strings.HasPrefix(cm.Repository.URL, "https://") && cm.ID != "" {
				x.URL = strings.TrimSuffix(cm.Repository.URL, "/") + "/commit/" + cm.ID
			}
			out = append(out, x)
		}
	}
	return out
}

func (t tools) comment(ctx context.Context, a extension.Authority, in BodyInput) (Done, error) {
	if strings.TrimSpace(in.Body) == "" {
		return Done{}, refuse("bad_request", "body must not be empty")
	}
	b, err := t.bind(ctx, a)
	if err != nil {
		return Done{}, err
	}
	issue, err := b.own()
	if err != nil {
		return Done{}, err
	}
	h := sdk.HostFrom(ctx)
	key := ""
	if id := extension.CallRequestID(ctx); id != "" {
		key = string(a.WorkID) + "/" + id
		if _, _, err := h.Get(ctx, CallsNS, key); err == nil {
			return Done{Issue: issue, Duplicate: true}, nil
		} else if !errors.Is(err, storage.ErrNotFound) {
			return Done{}, err
		}
	}
	if err := b.entry.client.Comment(ctx, issue, in.Body); err != nil {
		return Done{}, refusal(err)
	}
	if key != "" {
		_ = h.Commit(ctx, protocolPut(CallsNS, key, issue))
	}
	return Done{Issue: issue}, nil
}

func (t tools) resolve(ctx context.Context, a extension.Authority, _ struct{}) (Done, error) {
	b, err := t.bind(ctx, a)
	if err != nil {
		return Done{}, err
	}
	issue, err := b.own()
	if err != nil {
		return Done{}, err
	}
	if err := b.entry.client.Resolve(ctx, issue); err != nil {
		return Done{}, refusal(err)
	}
	return Done{Issue: issue}, nil
}

func protocolPut(ns, key, value string) protocol.KVCommitParams {
	return protocol.KVCommitParams{Puts: []protocol.KVPut{{Namespace: ns, Key: key, Value: []byte(value)}}}
}
