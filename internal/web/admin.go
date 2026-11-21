package web

import (
	"context"
	"errors"
	"net/http"
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
