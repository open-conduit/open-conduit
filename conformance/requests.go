package conformance

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

// RequestProvider is an action provider that reports its requests.
type RequestProvider interface {
	extension.ActionProvider
	extension.RequestProvider
}

// RequestFixture drives an extension.RequestProvider through the request
// contract (issue #681). Answerable is a target with an open request that
// one of its offered actions answers now. Others are further targets the
// rules must hold on too (one with a settled request, one with nothing
// outstanding).
type RequestFixture struct {
	Provider   RequestProvider
	Answerable extension.ActionTarget
	Others     []extension.ActionTarget
}

var requestStates = map[string]bool{
	extension.RequestOpen: true, extension.RequestAnswered: true, extension.RequestRefused: true,
	extension.RequestExpired: true, extension.RequestWithdrawn: true,
}

// CheckRequests verifies, for a request provider, on every target:
//   - each request has an ID unique on the target and a label;
//   - its State is one of the Request states;
//   - By and SettledAt are set only on a settled request;
//   - its Wording, if any, is for an approval (Wants) or a question
//     (Question), not both; each choice it words is worded once, with a
//     tone of affirm, danger or none, and on a request with an Action is
//     one of that action's choices;
//   - Action is set only on an open request, names an action offered on
//     the target now, and no two requests name the same action, so a
//     surface pairing a request with its action pairs the right one;
//
// and that Answerable reports an open request with its Action.
func CheckRequests(f RequestFixture) error {
	ctx := context.Background()
	check := func(t extension.ActionTarget) (int, error) {
		requests, err := f.Provider.Requests(ctx, t)
		if err != nil {
			return 0, err
		}
		actions, err := f.Provider.Actions(ctx, t)
		if err != nil {
			return 0, err
		}
		offered := map[string]bool{}
		choices := map[string][]string{}
		for _, a := range actions {
			offered[a.ID] = true
			choices[a.ID] = a.Choices
		}
		ids, named := map[string]bool{}, map[string]bool{}
		answerable := 0
		for _, r := range requests {
			switch {
			case strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.Label) == "":
				return 0, fmt.Errorf("request without an ID or label: %+v", r)
			case ids[r.ID]:
				return 0, fmt.Errorf("request ID %q reported twice", r.ID)
			case !requestStates[r.State]:
				return 0, fmt.Errorf("request %q has state %q", r.ID, r.State)
			case r.By != "" && r.State == extension.RequestOpen:
				return 0, fmt.Errorf("open request %q says who settled it", r.ID)
			case !r.SettledAt.IsZero() && r.State == extension.RequestOpen:
				return 0, fmt.Errorf("open request %q says when it was settled", r.ID)
			}
			if err := checkWording(r, choices); err != nil {
				return 0, err
			}
			ids[r.ID] = true
			if r.Action == "" {
				continue
			}
			switch {
			case r.State != extension.RequestOpen:
				return 0, fmt.Errorf("settled request %q names action %q", r.ID, r.Action)
			case !offered[r.Action]:
				return 0, fmt.Errorf("request %q names action %q, which is not offered", r.ID, r.Action)
			case named[r.Action]:
				return 0, fmt.Errorf("action %q is named by two requests", r.Action)
			}
			named[r.Action] = true
			answerable++
		}
		return answerable, nil
	}
	n, err := check(f.Answerable)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no open request with its action where one is answerable")
	}
	for _, t := range f.Others {
		if _, err := check(t); err != nil {
			return err
		}
	}
	return nil
}

// checkWording checks a request's Wording against the rules CheckRequests
// lists; choices are each offered action's.
func checkWording(r extension.Request, choices map[string][]string) error {
	w := r.Wording
	if w == nil {
		return nil
	}
	if w.Wants != "" && w.Question != "" {
		return fmt.Errorf("request %q is worded as an approval and a question", r.ID)
	}
	seen := map[string]bool{}
	for _, c := range w.Choices {
		switch {
		case seen[c.Choice]:
			return fmt.Errorf("request %q words choice %q twice", r.ID, c.Choice)
		case c.Tone != "" && c.Tone != extension.ToneAffirm && c.Tone != extension.ToneDanger:
			return fmt.Errorf("request %q gives choice %q the tone %q", r.ID, c.Choice, c.Tone)
		case r.Action != "" && !slices.Contains(choices[r.Action], c.Choice):
			return fmt.Errorf("request %q words choice %q, which action %q doesn't offer", r.ID, c.Choice, r.Action)
		}
		seen[c.Choice] = true
	}
	return nil
}

func RunRequests(t *testing.T, factory func() RequestFixture) {
	t.Helper()
	if err := CheckRequests(factory()); err != nil {
		t.Fatal(err)
	}
}
