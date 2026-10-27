package conformance

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

// ActionFixture drives an extension.ActionProvider through the action
// contract (issue #51). Action is offered on Available and not on
// Unavailable (nothing outstanding there). Caller may invoke it; Stranger
// may not. Input is valid; BadInput, when set, is refused. EventType is the
// family a granted invocation produces.
type ActionFixture struct {
	Provider    extension.ActionProvider
	Available   extension.ActionTarget
	Unavailable extension.ActionTarget
	Action      string
	Input       string
	BadInput    *string
	Caller      extension.Caller
	Stranger    extension.Caller
	EventType   string
}

var actionIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// CheckActions verifies, for an action provider:
//   - listing is well-formed (stable ids, labels, no duplicates) and offers
//     the action only where it is available;
//   - a granted invocation returns one Event of the expected family that
//     carries only what a plan may (no identity, source or causation), and
//     the same call plans the same Event, so a replay is safe;
//   - an unavailable target, a caller it does not recognise, invalid input
//     and an unknown action are each refused with an authored
//     *extension.OperationError, so the host accepts no Event.
func CheckActions(f ActionFixture) error {
	ctx := context.Background()
	listed := func(t extension.ActionTarget) (bool, error) {
		actions, err := f.Provider.Actions(ctx, t)
		if err != nil {
			return false, err
		}
		seen := map[string]bool{}
		found := false
		for _, a := range actions {
			if !actionIDPattern.MatchString(a.ID) || strings.TrimSpace(a.Label) == "" {
				return false, fmt.Errorf("malformed action %+v", a)
			}
			if seen[a.ID] {
				return false, fmt.Errorf("duplicate action %q", a.ID)
			}
			seen[a.ID] = true
			found = found || a.ID == f.Action
		}
		return found, nil
	}
	if ok, err := listed(f.Available); err != nil || !ok {
		return fmt.Errorf("action not offered where available: %v", err)
	}
	if ok, err := listed(f.Unavailable); err != nil || ok {
		return fmt.Errorf("action offered where nothing is outstanding: %v", err)
	}
	call := extension.ActionCall{Target: f.Available, Action: f.Action, Input: f.Input, Caller: f.Caller}
	plan, err := f.Provider.Invoke(ctx, call)
	if err != nil {
		return fmt.Errorf("granted invocation failed: %v", err)
	}
	e := plan.Event
	if e.Type != f.EventType || strings.HasPrefix(e.Type, "core.") {
		return fmt.Errorf("plan event type %q, want %q", e.Type, f.EventType)
	}
	if e.ID != "" || e.Source != "" || e.DeliveryID != "" || e.CausationID != "" || e.ExecutionID != "" || e.WorkID != "" {
		return fmt.Errorf("plan sets host-assigned event fields")
	}
	again, err := f.Provider.Invoke(ctx, call)
	if err != nil || !reflect.DeepEqual(plan, again) {
		return fmt.Errorf("invocation is not deterministic: %v", err)
	}
	refused := func(what string, c extension.ActionCall) error {
		_, err := f.Provider.Invoke(ctx, c)
		var oe *extension.OperationError
		if !errors.As(err, &oe) || !actionIDPattern.MatchString(oe.Code) || strings.TrimSpace(oe.Message) == "" {
			return fmt.Errorf("%s not refused with an authored code: %v", what, err)
		}
		return nil
	}
	unavailable := call
	unavailable.Target = f.Unavailable
	if err := refused("unavailable target", unavailable); err != nil {
		return err
	}
	stranger := call
	stranger.Caller = f.Stranger
	if err := refused("unrecognised caller", stranger); err != nil {
		return err
	}
	if f.BadInput != nil {
		bad := call
		bad.Input = *f.BadInput
		if err := refused("invalid input", bad); err != nil {
			return err
		}
	}
	unknown := call
	unknown.Action = "no_such_action"
	return refused("unknown action", unknown)
}

func RunActions(t *testing.T, factory func() ActionFixture) {
	t.Helper()
	if err := CheckActions(factory()); err != nil {
		t.Fatal(err)
	}
}
