package domain_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
)

func TestTurnOfReadsOnlyTheCoreSchema(t *testing.T) {
	data, _ := json.Marshal(domain.TurnDeclaration{Status: "approved", Comment: "Looks good"})
	for schema, want := range map[string]bool{domain.TurnSchema: true, "github.status.v1": false, "jira.status.v1": false, "open-conduit.claudecode.result.v1": false, "": false} {
		d, ok := domain.TurnOf(&domain.Outcome{Kind: domain.OutcomeHandoff, Data: domain.Payload{Schema: schema, Data: data}})
		if ok != want || ok && (d.Status != "approved" || d.Comment != "Looks good") {
			t.Errorf("%q: %+v %v, want %v", schema, d, ok, want)
		}
	}
	if _, ok := domain.TurnOf(nil); ok {
		t.Error("no outcome declared a turn")
	}
}

func TestValidTurnStatusChecksShapeNotVocabulary(t *testing.T) {
	for s, want := range map[string]bool{
		"review": true, "finished": true, "approved": true, "needs-qa": true, "stage_2": true, "a": true,
		"": false, "Review": false, "2nd": false, "-x": false, "has space": false, "a:b": false, "a/b": false,
		strings.Repeat("a", domain.MaxTurnStatus): true, strings.Repeat("a", domain.MaxTurnStatus+1): false,
	} {
		if domain.ValidTurnStatus(s) != want {
			t.Errorf("%q valid=%v, want %v", s, !want, want)
		}
	}
}
