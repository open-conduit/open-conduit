package extension

import (
	"fmt"
	"regexp"
)

// Pages (issue #242). An extension may declare a page of its own in the
// operator's web view: a session of one of its Work kinds, started from a
// scope with one of its entries. Core draws the page from what every
// extension already has, so no extension code or markup reaches the
// browser:
//
//   - with no session open, the entry's Start action on a scope that
//     offers it, with the grants the binding of the entry's route offers
//     for the operator to turn on (each one turned on is added to the
//     start input as "+name");
//   - with a session open, the session Work's panels: a panel section of
//     Messages is the conversation, the rest stand beside it;
//   - one message box that sends the first of Compose the entry offers on
//     the session, its choices as buttons;
//   - the session's configuration change set, when the extension proposes
//     one (ConfigCandidate): each change's before and after, approve and
//     reject, and Apply, which only the operator can do.

// Page limits.
const (
	MaxPages       = 4
	MaxPageTitle   = 60
	MaxPageSummary = 400
	MaxPageCompose = 4
)

// Page is a page an extension contributes. ID ([a-z][a-z0-9_-]*) is the
// page's path under /pages/ and unique among every extension's pages.
// Entry is the extension entry type whose scopes offer the page, and whose
// entry is the action provider of every action named here. WorkKind is the
// kind of the Work a session is. Start is the action on the scope that
// starts one, and Compose the actions on the session the message box sends,
// the first offered winning (an answer before a message, say). Route,
// when set, is the route handler whose binding's grants the operator may
// turn on when starting.
type Page struct {
	ID       string
	Title    string
	Summary  string
	Entry    string
	WorkKind string
	Start    string
	Compose  []string
	Route    string
}

var pageID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Validate reports why the page cannot be offered, or nil.
func (p Page) Validate() error {
	if !pageID.MatchString(p.ID) {
		return fmt.Errorf("page id %q is not [a-z][a-z0-9_-]*", p.ID)
	}
	if err := line("page title", p.Title, MaxPageTitle, true); err != nil {
		return err
	}
	if err := text("page summary", p.Summary, MaxPageSummary, false); err != nil {
		return err
	}
	if p.Entry == "" || p.WorkKind == "" {
		return fmt.Errorf("page %s needs an entry type and a work kind", p.ID)
	}
	if !panelID.MatchString(p.Start) {
		return fmt.Errorf("page %s start %q is not an action id", p.ID, p.Start)
	}
	if len(p.Compose) > MaxPageCompose {
		return fmt.Errorf("page %s has more than %d compose actions", p.ID, MaxPageCompose)
	}
	for _, a := range p.Compose {
		if !panelID.MatchString(a) {
			return fmt.Errorf("page %s compose %q is not an action id", p.ID, a)
		}
	}
	return nil
}
