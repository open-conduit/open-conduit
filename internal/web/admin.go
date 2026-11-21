package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/client"
)

// The admin pages put what an operator would otherwise need a shell on the
// daemon's machine for in the browser: secrets, operator credentials and
// doctor with secrets checked. Each is a management call made as the person
// signed in, so the daemon's role check is the only gate: the view decides
// nothing about who may do what, and shows the daemon's refusal.
//
// A secret's value goes from the form to PUT /v1/secrets/{name} and nowhere
// else: never into a flash, a receipt, a redirect or a page. An issued token
// is drawn once, in the response to the POST that issued it (every response
// is no-store), and never travels in a flash cookie.

// doctorTimeout bounds doctor with secrets checked, which tries each
// credential against its service.
const doctorTimeout = 45 * time.Second

// adminView is one admin page.
type adminView struct {
	Tab       string
	Refused   string
	Secrets   []client.Secret
	Operators []client.Operator
	Issued    *client.IssuedOperator
	IssuedTo  string
	Doctor    *client.Doctor
	Checked   bool
}

// adminRender draws tab, turning the daemon's role refusal into the page's
// own state with its status.
func (s *Server) adminRender(w http.ResponseWriter, r *http.Request, title string, v adminView, err error) {
	p := &page{Title: title, Nav: "admin"}
	if err != nil {
		var e *client.Error
		if errors.As(err, &e) && e.Status == http.StatusForbidden {
			v.Refused, p.Status = e.Message, http.StatusForbidden
		} else {
			p.Err = errText(err)
		}
	}
	s.render(w, r, "admin", p, v)
}

func (s *Server) secretsPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	list, err := s.src.Secrets(ctx)
	s.adminRender(w, r, "Secrets", adminView{Tab: "secrets", Secrets: list}, err)
}

// storeSecret stores the form's value. Only the name is ever repeated.
func (s *Server) storeSecret(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	name := strings.TrimSpace(r.PostFormValue("name"))
	value := r.PostFormValue("value")
	if name == "" || value == "" {
		flash(w, "A secret needs a name and a value.", true)
		http.Redirect(w, r, "/secrets", http.StatusSeeOther)
		return
	}
	out, err := s.src.SetSecret(ctx, name, value, r.PostFormValue("store"))
	if err != nil {
		// Not the name: a value pasted into the wrong box must not come back.
		flash(w, "Could not store the secret: "+errText(err), true)
	} else {
		flash(w, "Stored "+out.Name+" as "+out.Reference+".", false)
	}
	http.Redirect(w, r, "/secrets", http.StatusSeeOther)
}

func (s *Server) removeSecret(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	name := r.PostFormValue("name")
	if err := s.src.DeleteSecret(ctx, name); err != nil {
		flash(w, "Could not remove "+name+": "+errText(err), true)
	} else {
		flash(w, "Removed "+name+".", false)
	}
	http.Redirect(w, r, "/secrets", http.StatusSeeOther)
}

// answerSecret takes the value an extension's action asked for. The name
// comes from the daemon's own listing of the action, never the form. The
// value goes only to the secret store (admin, checked by the daemon), and
// the action is invoked with the name. A name already stored isn't
// replaced from here, since whoever wrote the prompt chose the name: an
// empty value uses the stored one, and replacing it is the Secrets page's.
func (s *Server) answerSecret(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	t := client.ActionTarget{ScopeID: r.PostFormValue("scope"), WorkID: r.PostFormValue("work")}
	provider, id, value := r.PostFormValue("provider"), r.PostFormValue("action"), r.PostFormValue("value")
	fail := func(text string) {
		flash(w, text, true)
		http.Redirect(w, r, back(r), http.StatusSeeOther)
	}
	if (t.ScopeID == "") == (t.WorkID == "") {
		http.Error(w, "an action needs a scope or Work", http.StatusBadRequest)
		return
	}
	offered, err := s.src.Actions(ctx, t)
	if err != nil {
		fail(actionsErr(err))
		return
	}
	i := slices.IndexFunc(offered, func(a client.Action) bool { return a.Provider == provider && a.ID == id && a.Secret != "" })
	if i < 0 {
		fail("That request for a secret is no longer open.")
		return
	}
	a := offered[i]
	stored, err := s.src.Secrets(ctx)
	if err != nil {
		fail("Storing a secret needs the admin role: " + errText(err))
		return
	}
	exists := slices.ContainsFunc(stored, func(x client.Secret) bool { return x.Name == a.Secret && x.Resolves })
	switch {
	case value == "" && !exists:
		fail("Enter a value for " + a.Secret + ".")
		return
	case value != "" && exists:
		fail(a.Secret + " is already stored. Leave the value empty to use it, or replace it on the Secrets page first.")
		return
	case value != "":
		if _, err := s.src.SetSecret(ctx, a.Secret, value, ""); err != nil {
			fail("Could not store " + a.Secret + ": " + errText(err))
			return
		}
	}
	res, err := s.src.InvokeAction(ctx, t, provider, id, a.Secret, s.opts.NewKey())
	rc := receipt{Summary: a.Label + " · " + a.Secret, Call: "PUT /v1/secrets/" + a.Secret, Event: res.Event.ID, WorkID: t.WorkID}
	text := "Stored " + a.Secret + "."
	if res.Message != "" {
		text = res.Message
	}
	s.record(w, r, text, rc, err)
}

func (s *Server) operatorsPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	list, err := s.src.Operators(ctx)
	s.adminRender(w, r, "Operators", adminView{Tab: "operators", Operators: list}, err)
}

// issueOperator issues a credential and draws its token in this response,
// the only time it is shown.
func (s *Server) issueOperator(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	name := strings.TrimSpace(r.PostFormValue("name"))
	out, err := s.src.IssueOperator(ctx, client.OperatorRequest{Name: name, Role: r.PostFormValue("role"), TTL: strings.TrimSpace(r.PostFormValue("ttl"))})
	if err != nil {
		var e *client.Error
		if !errors.As(err, &e) || e.Status != http.StatusForbidden {
			flash(w, "Could not issue a credential for "+name+": "+errText(err), true)
			http.Redirect(w, r, "/operators", http.StatusSeeOther)
			return
		}
		s.adminRender(w, r, "Operators", adminView{Tab: "operators"}, err)
		return
	}
	list, lerr := s.src.Operators(ctx)
	s.adminRender(w, r, "Operators", adminView{Tab: "operators", Operators: list, Issued: &out, IssuedTo: name}, lerr)
}

func (s *Server) revokeOperator(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	id := r.PostFormValue("token_id")
	if _, err := s.src.Revoke(ctx, id, formReason(r, "revoked from the web view"), s.opts.NewKey()); err != nil {
		flash(w, "Could not revoke: "+errText(err), true)
	} else {
		flash(w, "Revoked credential "+id+".", false)
	}
	http.Redirect(w, r, "/operators", http.StatusSeeOther)
}

func (s *Server) doctorPage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := call(r)
	defer cancel()
	v := adminView{Tab: "doctor"}
	d, err := s.src.Doctor(ctx, false)
	if err == nil {
		v.Doctor = &d
	}
	s.adminRender(w, r, "Doctor", v, err)
}

// checkDoctor runs doctor with secrets checked. It is a POST because it
// resolves every reference and tries each credential against its service.
func (s *Server) checkDoctor(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), doctorTimeout)
	defer cancel()
	d, err := s.src.Doctor(ctx, true)
	if err != nil {
		s.adminRender(w, r, "Doctor", adminView{Tab: "doctor"}, err)
		return
	}
	s.adminRender(w, r, "Doctor", adminView{Tab: "doctor", Doctor: &d, Checked: true}, nil)
}
