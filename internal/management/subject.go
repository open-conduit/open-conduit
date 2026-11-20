package management

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Acting for a subject (issue #687). A client that serves many people (a
// company's own UI, the bundled web view behind a sign-in proxy) holds one
// credential and names, on each request, the person it acts for. The host
// checks that the credential may act for that kind of subject, records
// the subject as the Event's actor and the client as what it came through
// (Event.Via), and authorises the request as both:
//
//   - A subject of the client's own kind (acme-ui:alice, from a credential
//     with act_for acme-ui) has no operator role or grants. It may invoke
//     actions, which their provider authorises as it would any caller, and
//     nothing else.
//   - An operator subject, from a credential trusted to assert operator
//     identities (act_for operator), is either a named operator
//     (operator:alice), whose role and grants are those of its active
//     credentials, or whoever an operator credential sent in
//     SubjectTokenHeader verifies as (an identity provider's token). Its
//     authority is its own intersected with the client's.
//
// Signing the person in is the client's: the host never runs a login.
const (
	// SubjectHeader names the subject a request acts for, as kind:id.
	SubjectHeader = "Open-Conduit-Subject"
	// SubjectTokenHeader carries the subject's own operator credential
	// (one the host issued, or a trusted issuer's token) in place of a
	// name: the subject is whoever it authenticates as.
	SubjectTokenHeader = "Open-Conduit-Subject-Token"
)

// maxSubjectID bounds a subject's ID, as Host.Act bounds a caller's.
const maxSubjectID = 200

// actFor resolves the subject the request names, if it names one, into
// who.subject, refusing it unless the credential may act for it and the
// route is one a subject may take.
func (s *Server) actFor(ctx context.Context, r *http.Request, who principal, rt *route) (principal, error) {
	name, token := r.Header.Get(SubjectHeader), r.Header.Get(SubjectTokenHeader)
	if name == "" && token == "" {
		return who, nil
	}
	if !who.operator {
		return who, fail(403, "forbidden_subject", "an execution acts only as itself: send no "+SubjectHeader)
	}
	if name != "" && token != "" {
		return who, fail(400, "malformed", "name the subject in "+SubjectHeader+" or send its credential in "+SubjectTokenHeader+", not both")
	}
	if who.actFor == "" {
		return who, fail(403, "forbidden_subject", "operator "+who.name+" may not act for anyone: its credential has no act_for")
	}
	switch strings.Join(rt.pattern, "/") {
	case "auth/rotate", "auth/revoke":
		return who, fail(403, "subject_not_permitted", "this route changes the client's own credential: call it without "+SubjectHeader)
	}
	var sub principal
	var err error
	if token != "" {
		sub, err = s.subjectByToken(ctx, who, token)
	} else {
		sub, err = s.subjectByName(ctx, who, name)
	}
	if err != nil {
		return who, err
	}
	if !sub.operator && !subjectRoute(rt) {
		return who, fail(403, "subject_not_permitted", sub.caller().String()+" holds no operator role or grants, so it may only invoke actions, which their provider authorises; make other calls without "+SubjectHeader)
	}
	sub.token, sub.tokenID, sub.actFor = "", "", ""
	who.subject = &sub
	return who, nil
}

// subjectRoute reports whether a subject without operator authority may
// take rt: invoking an action, which its provider authorises.
func subjectRoute(rt *route) bool {
	n := len(rt.pattern)
	return rt.method == http.MethodPost && n == 5 && rt.pattern[n-3] == "actions"
}

// subjectByToken is the operator a subject's own credential authenticates
// as: one the host issued, or a trusted issuer's token. Only a client
// trusted to assert operator identities may send one.
func (s *Server) subjectByToken(ctx context.Context, who principal, token string) (principal, error) {
	if who.actFor != extension.CallerOperator {
		return principal{}, fail(403, "forbidden_subject", "operator "+who.name+" may act only for "+who.actFor+" subjects, not operators")
	}
	if len(token) > maxBearer || strings.ContainsAny(token, " \t\r\n") {
		return principal{}, fail(401, "subject_unauthenticated", "the subject's credential is not one this daemon accepts")
	}
	sub, found, err := s.issuedOperator(ctx, token)
	if !found && err == nil {
		sub, err = s.verifyIssued(ctx, token)
	}
	if err != nil {
		if api := mapError(err); api.status == 401 {
			return principal{}, fail(401, "subject_unauthenticated", "the subject's credential: "+api.message)
		}
		return principal{}, err
	}
	if sub.local() {
		return principal{}, fail(403, "forbidden_subject", "the owner's own credential is never a subject")
	}
	return sub, nil
}

// subjectByName is the subject kind:id names: an operator by name, for a
// client trusted to assert operator identities, or one of the client's
// own kind, which has no operator authority.
func (s *Server) subjectByName(ctx context.Context, who principal, name string) (principal, error) {
	kind, id, ok := strings.Cut(name, ":")
	if !ok || kind == "" || !validSubjectID(id) {
		return principal{}, fail(400, "malformed", SubjectHeader+" is kind:id, the id 1 to 200 printable characters")
	}
	if kind != who.actFor {
		return principal{}, fail(403, "forbidden_subject", "operator "+who.name+" may act only for "+who.actFor+" subjects, not "+kind)
	}
	if kind != extension.CallerOperator {
		// The kind may have become a package's since the credential was
		// issued: a package's people are its to vouch for (Host.Act).
		if err := s.checkActFor(kind); err != nil {
			return principal{}, fail(403, "forbidden_subject", "an installed package acts for "+kind+" subjects, so no client may")
		}
		return principal{kind: kind, name: id}, nil
	}
	if strings.Contains(id, ":") {
		return principal{}, fail(403, "forbidden_subject", "an identity provider's operator is named by its own token: send it in "+SubjectTokenHeader)
	}
	if !ValidOperatorName(id) {
		return principal{}, fail(403, "forbidden_subject", "operator "+id+" can't be a subject: only a named operator can, never the owner's own")
	}
	return s.namedOperator(ctx, id)
}

// namedOperator is the operator name as a subject: the highest role and
// every grant among its active credentials. One with none is refused, so
// revoking someone's credentials stops a client acting for them too.
func (s *Server) namedOperator(ctx context.Context, name string) (principal, error) {
	p := principal{operator: true, name: name}
	found := false
	err := s.read(ctx, func(tx storage.Tx) error {
		rows, err := tx.List(authNamespace, "operator/")
		if err != nil {
			return err
		}
		now := s.o.Now()
		best := map[string]string{}
		for _, row := range rows {
			var r operatorRecord
			if err = json.Unmarshal(row.Value, &r); err != nil {
				return err
			}
			if r.Name != name || !r.active(now) {
				continue
			}
			found = true
			if roleRank[r.role()] > roleRank[p.role] {
				p.role = r.role()
			}
			for _, g := range r.Grants {
				if roleRank[g.Role] > roleRank[best[string(g.Scope)]] {
					best[string(g.Scope)] = g.Role
				}
			}
		}
		for scope, role := range best {
			p.grants = append(p.grants, Grant{Scope: domain.ID(scope), Role: role})
		}
		slices.SortFunc(p.grants, func(a, b Grant) int { return strings.Compare(string(a.Scope), string(b.Scope)) })
		return nil
	})
	if err != nil {
		return principal{}, err
	}
	if !found {
		return principal{}, fail(403, "forbidden_subject", "operator "+name+" holds no active credential, so no client may act for it")
	}
	return p, nil
}

// checkActFor refuses a kind no credential may act for: one that isn't a
// lower case name, or that an installed package acts for itself.
// "operator" is allowed: it trusts the credential to assert operator
// identities.
func (s *Server) checkActFor(kind string) error {
	if !operatorName.MatchString(kind) {
		return fail(400, "malformed", "act_for is a subject kind: 1-32 lower case letters, digits or dashes, starting with a letter")
	}
	if s.o.ExtensionKind != nil && s.o.ExtensionKind(kind) {
		return fail(409, "act_for_taken", "the installed package "+kind+" acts for "+kind+" subjects itself; choose a kind of your own, such as your frontend's name")
	}
	return nil
}

func validSubjectID(id string) bool {
	if id == "" || len(id) > maxSubjectID {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}
