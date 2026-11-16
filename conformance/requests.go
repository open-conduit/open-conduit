package conformance

import (
	"context"
	"fmt"
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
//   - By is set only on a settled request;
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
		for _, a := range actions {
			offered[a.ID] = true
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

func RunRequests(t *testing.T, factory func() RequestFixture) {
	t.Helper()
	if err := CheckRequests(factory()); err != nil {
		t.Fatal(err)
	}
}
