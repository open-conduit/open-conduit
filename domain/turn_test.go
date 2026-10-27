package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
)

func TestTurnOfReadsTheCoreAndLegacySchemas(t *testing.T) {
	data, _ := json.Marshal(domain.TurnDeclaration{Status: "review", Comment: "PR is up"})
	for schema, want := range map[string]bool{domain.TurnSchema: true, "github.status.v1": true, "linear.status.v1": true, "jira.status.v1": true, ".status.v1": false, "open-conduit.claudecode.result.v1": false, "": false} {
		d, ok := domain.TurnOf(&domain.Outcome{Kind: domain.OutcomeHandoff, Data: domain.Payload{Schema: schema, Data: data}})
		if ok != want || ok && (d.Status != "review" || d.Comment != "PR is up") {
			t.Errorf("%q: %+v %v, want %v", schema, d, ok, want)
		}
	}
	if _, ok := domain.TurnOf(nil); ok {
		t.Error("no outcome declared a turn")
	}
	for s, want := range map[string]bool{"building": true, "review": true, "help": true, "done": true, "finished": true, "waiting": false, "": false} {
		if domain.ValidTurnState(s) != want {
			t.Errorf("%q valid=%v", s, !want)
		}
	}
}
