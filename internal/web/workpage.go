package web

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli/runview"
)

type runData struct {
	Run      client.Run
	Detail   string
	Attempts []client.Run
	// Turns are the Work's turns as a timeline, the run shown among them
	// with its conversation.
	Turns  []turnView
	Work   *client.Work
	Waits  []client.Wait
	Panels []panelView
	// Actions are those the panels do not already show.
	Actions []client.Action
}

// workPage is the Work page (10-05). Placeholder until the port lands: it
// shows the Work's latest turn the old way.
func (s *Server) workPage(w http.ResponseWriter, r *http.Request) {
	s.runPage(w, r)
}

// runRedirect sends the old run URL to its Work and turn.
func (s *Server) runRedirect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	run, err := s.src.Run(ctx, r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, errText(err), statusOf(err))
		return
	}
	http.Redirect(w, r, "/work?"+url.Values{"id": {run.WorkID}, "turn": {itoa(int(run.Attempt))}}.Encode(), http.StatusSeeOther)
}

func (s *Server) runPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	p := &page{Title: "Run", Nav: "runs"}
	run, err := s.src.Run(ctx, r.URL.Query().Get("id"))
	if err != nil {
		p.Err = errText(err)
		w.WriteHeader(statusOf(err))
		s.render(w, r, "work", p, nil)
		return
	}
	d := runData{Run: run, Detail: runview.Detail(run, nil, s.opts.Now())}
	p.Title = run.WorkID
	var errs []string
	if page, err := s.src.Runs(ctx, url.Values{"work_id": {run.WorkID}, "limit": {"200"}}); err != nil {
		errs = append(errs, errText(err))
	} else {
		d.Attempts = page.Items
		sort.SliceStable(d.Attempts, func(i, j int) bool { return d.Attempts[i].Attempt < d.Attempts[j].Attempt })
	}
	if work, err := s.src.Work(ctx, run.WorkID); err != nil {
		errs = append(errs, errText(err))
	} else {
		d.Work = &work
	}
	if page, err := s.src.Waits(ctx, url.Values{"work_id": {run.WorkID}, "limit": {"200"}}); err != nil {
		errs = append(errs, errText(err))
	} else {
		for _, wt := range page.Items {
			// As in the TUI: the open Waits, and those that woke the Work.
			if wt.State == "active" || wt.WakeEventID != "" {
				d.Waits = append(d.Waits, wt)
			}
		}
		sort.SliceStable(d.Waits, func(i, j int) bool { return d.Waits[i].CreatedAt < d.Waits[j].CreatedAt })
	}
	if len(d.Attempts) == 0 {
		d.Attempts = []client.Run{run}
	}
	for _, a := range d.Attempts {
		if a.ID == run.ID {
			d.Turns = append(d.Turns, s.turn(ctx, run))
		} else {
			d.Turns = append(d.Turns, turnView{Run: a, Live: runview.Running(a)})
		}
	}
	target := client.ActionTarget{WorkID: run.WorkID}
	if d.Panels, err = s.panels(ctx, p, target); err != nil {
		errs = append(errs, errText(err))
	}
	if actions, err := s.src.Actions(ctx, target); err != nil {
		errs = append(errs, actionsErr(err))
	} else {
		d.Actions = unshown(actions, d.Panels)
	}
	p.Err = strings.Join(errs, "\n")
	s.render(w, r, "work", p, d)
}

// turnView is one turn in a run's timeline. Conv is nil until it is
// read: the run shown is read with the page, the others when opened.
type turnView struct {
	Run  client.Run
	Live bool
	Conv *conversation
	// Missing says why the turn has no conversation to show.
	Missing string
}

// turn reads a turn's conversation from its transcript: the live one while
// it runs, else its artifact.
func (s *Server) turn(ctx context.Context, run client.Run) turnView {
	t := turnView{Run: run, Live: runview.Running(run)}
	out, err := s.fetchOutput(ctx, run.ID, pickTranscript, TranscriptTail)
	if err != nil {
		t.Missing = errText(err)
		return t
	}
	head, skipped, tail := out.Text, int64(0), ""
	if cut := out.Size - int64(len(out.Text)); cut > 0 && out.Artifact != "" {
		// An ended turn's transcript that is too long to read whole: its
		// start too, to show what the agent was told.
		var b strings.Builder
		if err := s.src.ArtifactContent(ctx, out.Artifact, "bytes=0-"+strconv.FormatInt(min(cut, TranscriptHead)-1, 10), &b); err == nil {
			head, skipped, tail = b.String(), cut-int64(b.Len()), out.Text
			if skipped <= 0 {
				head, skipped, tail = head+out.Text, 0, ""
			}
		}
	}
	c := parseConversation(head, skipped, tail)
	if head == out.Text && out.Size > int64(len(out.Text)) {
		c.Trimmed = out.Size - int64(len(out.Text))
	}
	if !out.Live {
		settle(c.Entries)
	}
	t.Live, t.Conv = out.Live, &c
	return t
}

// conversationPart is one turn's conversation alone, for the page to load
// a turn when it is opened and to follow one that is running. X-Live says
// whether it is still running.
func (s *Server) conversationPart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	run, err := s.src.Run(ctx, r.URL.Query().Get("run"))
	if err != nil {
		http.Error(w, errText(err), statusOf(err))
		return
	}
	t := s.turn(ctx, run)
	live := "0"
	if t.Live {
		live = "1"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Live", live)
	if err := s.pages["work"].ExecuteTemplate(w, "conversation", t); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// actionsErr explains a daemon too old to list actions.
