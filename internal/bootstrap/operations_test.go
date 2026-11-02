package bootstrap

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
)

// Every operation a first-party extension declares says what it does and
// what it takes, so the operations reference and the agents granted it can
// show both.
func TestFirstPartyOperationsDescribed(t *testing.T) {
	for _, x := range firstParty() {
		o, ok := x.(inproc.Operator)
		if !ok {
			continue
		}
		for _, op := range o.Operations() {
			for _, problem := range describedProblems(op) {
				t.Errorf("%s: %s", op.Name, problem)
			}
		}
	}
}

// The check catches an undescribed operation, one with an empty input
// schema and one whose summary isn't a sentence.
func TestFirstPartyOperationsDescribedNegative(t *testing.T) {
	for _, op := range []extension.Operation{
		{Name: "x.none"},
		{Name: "x.summary", Summary: "Do it."},
		{Name: "x.input", Input: extension.InputSchema[struct{}]()},
		{Name: "x.sentence", Summary: "do it", Input: extension.InputSchema[struct{}]()},
		{Name: "x.lines", Summary: "Do it.\nThen more.", Input: extension.InputSchema[struct{}]()},
	} {
		if len(describedProblems(op)) == 0 {
			t.Errorf("%s passed", op.Name)
		}
	}
	if p := describedProblems(extension.Operation{Name: "x.ok", Summary: "Do it.", Input: extension.InputSchema[struct{}]()}); len(p) != 0 {
		t.Errorf("a described operation failed: %v", p)
	}
}

func describedProblems(op extension.Operation) []string {
	var out []string
	if err := extension.ValidateOperationDoc(op.Summary, op.Input); err != nil {
		out = append(out, err.Error())
	}
	s := op.Summary
	if s == "" || !unicode.IsUpper([]rune(s)[0]) || !strings.HasSuffix(s, ".") {
		out = append(out, "summary must be a sentence: a capital first, a full stop last")
	}
	var schema map[string]any
	if json.Unmarshal(op.Input, &schema) != nil || schema["type"] != "object" {
		out = append(out, "input must be declared as an object schema (extension.InputSchema)")
	}
	return out
}
