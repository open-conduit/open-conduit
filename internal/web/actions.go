package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-conduit/open-conduit/client"
)

func (s *Server) workControl(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	controls := map[string]func(ctx context.Context, id, revision, reason, key string) (client.ControlResult, error){
		"retry":   s.src.RetryWork,
		"hold":    s.src.HoldWork,
		"release": s.src.ReleaseWork,
		"cancel":  s.src.CancelWork,
	}
	name := r.PathValue("control")
	fn, ok := controls[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	work, err := s.src.Work(ctx, r.PostFormValue("work"))
	if err != nil {
		s.done(w, r, "", err)
		return
	}
	res, err := fn(ctx, work.ID, work.Revision, reason, s.opts.NewKey())
	s.done(w, r, name+" accepted (event "+res.EventID+")", err)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	run, err := s.src.Run(ctx, r.PostFormValue("run"))
	if err != nil {
		s.done(w, r, "", err)
		return
	}
	res, err := s.src.CancelExecution(ctx, run.ID, run.Revision, reason, s.opts.NewKey())
	s.done(w, r, "execution cancel accepted (event "+res.EventID+")", err)
}

// invokeAction invokes an action an extension offers on a scope or Work,
// as the local operator.
func (s *Server) invokeAction(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	t := client.ActionTarget{ScopeID: r.PostFormValue("scope"), WorkID: r.PostFormValue("work")}
	if (t.ScopeID == "") == (t.WorkID == "") {
		http.Error(w, "an action needs a scope or Work", http.StatusBadRequest)
		return
	}
	label := r.PostFormValue("label")
	res, err := s.src.InvokeAction(ctx, t, r.PostFormValue("provider"), r.PostFormValue("action"), strings.TrimSpace(r.PostFormValue("input")), s.opts.NewKey())
	text := label + " accepted"
	if res.Message != "" {
		text = res.Message
	}
	s.done(w, r, text+" (event "+res.Event.ID+")", err)
}

// confirmPage confirms an action without script (10-07). Placeholder.
func (s *Server) confirmPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "confirm", &page{Title: "Confirm", Nav: "runs"}, nil)
}
