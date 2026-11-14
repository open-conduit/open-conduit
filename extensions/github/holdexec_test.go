package github_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	gh "github.com/open-conduit/open-conduit/extensions/github"
)

func holdRequest(endpoint string) extension.RunRequest {
	return extension.RunRequest{
		Work:      domain.Work{ID: "w", ScopeID: "eng", Kind: gh.WorkKindHeld},
		Execution: domain.Execution{ID: "x1", WorkID: "w", Attempt: 1, Lease: domain.Lease{Epoch: 1}},
		Trigger:   domain.Event{ID: "t", Type: "github.issue.open"},
		Context:   extension.ExecutionContext{ToolbeltEndpoint: endpoint, Config: domain.Payload{Schema: "github.hold.v1", Data: json.RawMessage(`{}`)}},
	}
}

// The hold executor meets the shared executor contract: it persists its
// recovery state before acknowledging, adopts its own attempt with the same
// result, refuses an unknown recovery state and a failed persistence. An
// attempt with no toolbelt endpoint fails with a reason rather than
// refusing to launch.
func TestHoldExecutorConformance(t *testing.T) {
	conformance.RunExecutor(t, func() extension.Executor { return gh.NewHoldExecutor() }, holdRequest(""))
	x := gh.NewHoldExecutor()
	r := holdRequest("")
	r.PersistRecovery = func(domain.Payload) error { return nil }
	h, err := x.Run(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Wait(context.Background())
	if err != nil || res.Outcome.Kind != domain.OutcomeFailed || res.Outcome.Summary == "" {
		t.Fatalf("without a toolbelt: %+v %v", res, err)
	}
}

// fakeBelt answers the hold's calls for one credential: refused (with the
// management endpoint's code) or as a still-held issue.
type fakeBelt struct{ refuse string }

func (f fakeBelt) ToolbeltCall(_ context.Context, c client.ToolCall) (client.ToolResult, error) {
	if f.refuse != "" {
		return client.ToolResult{}, &client.Error{Status: 403, Code: f.refuse, Message: "refused"}
	}
	out, _ := json.Marshal(gh.HoldStatus{Held: true, Describe: "the match"})
	return client.ToolResult{Output: &client.Payload{Schema: "github.hold.status.v1", Data: out}}, nil
}

// A decision refused because the attempt's lease lapsed (forbidden, or
// authority_recovering where the host says so) is decided again when
// recovery hands the attempt the new epoch's credential, after
// re-acknowledging its recovery state.
func TestHoldExecutorDecidesAgainUnderTheRecoveredCredential(t *testing.T) {
	for _, code := range []string{"forbidden", "authority_recovering"} {
		t.Run(code, func(t *testing.T) {
			x := gh.NewHoldExecutor()
			x.Dial = func(_, token string) (gh.ToolbeltClient, error) {
				if token == "old" {
					return fakeBelt{refuse: code}, nil
				}
				return fakeBelt{}, nil
			}
			r := holdRequest("unix:///belt.sock")
			r.Context.ToolbeltToken = "old"
			var state domain.Payload
			r.PersistRecovery = func(p domain.Payload) error { state = p; return nil }
			h, err := x.Run(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.Wait(context.Background()); err == nil {
				t.Fatal("the lapsed credential's decision did not fail")
			}
			r.Context.ToolbeltToken, r.Execution.Lease.Epoch = "new", 2
			reacked := false
			r.PersistRecovery = func(p domain.Payload) error { reacked = p.Schema == state.Schema; return nil }
			adopted, err := x.Recover(context.Background(), r, state)
			if err != nil || !reacked {
				t.Fatalf("recover: %v (re-acknowledged %v)", err, reacked)
			}
			res, err := adopted.Wait(context.Background())
			if err != nil || res.Outcome.Kind != domain.OutcomeHandoff || res.Outcome.Summary != "held: waiting for the match" {
				t.Fatalf("recovered decision: %+v %v", res, err)
			}
		})
	}
}
