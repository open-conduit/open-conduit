package github_test

import (
	"context"
	"encoding/json"
	"testing"

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
