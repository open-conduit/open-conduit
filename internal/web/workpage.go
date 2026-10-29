package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/internal/cli/runview"
)

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

// turnView is one turn's transcript as the turn panel shows it: its chat,
// filtered by the toolbar's query string. Conv is nil when there is no
// transcript to read, and Missing says why.
type turnView struct {
	Run  client.Run
	Live bool
	Conv *conversation
	Chat chat
	// Blocks are the chat's blocks the toolbar shows.
	Blocks []chatBlock
	// Missing says why the turn has no conversation to show.
	Missing string
	View    txView
}

// txView is the transcript toolbar's state, from the query string: the
// filters on, the text to find and whether every row is open.
type txView struct {
	On     map[string]bool
	Query  string
	Expand bool
}

func txViewOf(q url.Values) txView {
	v := txView{On: map[string]bool{}, Query: strings.TrimSpace(q.Get("q")), Expand: q.Get("expand") == "1"}
	show, set := q["show"]
	for _, f := range chatFilters {
		v.On[f] = !set
	}
	if set {
		for _, part := range strings.Split(strings.Join(show, ","), ",") {
			v.On[part] = true
		}
	}
	return v
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
	t.Chat = buildChat(c, run)
	return t
}

// filter applies the toolbar to the turn's chat.
func (t *turnView) filter(v txView) {
	t.View = v
	t.Blocks = t.Chat.show(v.On, v.Query)
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
	t.filter(txViewOf(r.URL.Query()))
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
