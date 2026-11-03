package extension

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// A Toolset is a named set of toolbelt operations an extension publishes for
// a role its agents play (a GitHub issue's author, a PR's reviewer). Config
// grants it by name, as "@<name>" in a scope's authorization.operations or a
// binding's operations, so a release that adds an operation to the role adds
// it to every install granting the toolset, without a config change.
//
// Name is in the extension's namespace (Name + "." + a suffix, like its
// operations). Each operation records the toolset Version it joined in
// (Since, 1 for the first), so the host can tell an operator who granted an
// earlier version's operations by name which ones they are missing. An
// operation is one of the extension's own or a builtin; a toolset never
// grants another extension's operations. Operations are only ever added: a
// toolset keeps the ones it has published.
type Toolset struct {
	Name, Summary string
	Operations    []ToolsetOperation
}

// ToolsetOperation is one operation of a toolset and the version it joined
// the toolset in.
type ToolsetOperation struct {
	Name  string `json:"name"`
	Since int    `json:"since,omitempty"`
}

// Version is the toolset's current version: the latest any of its
// operations joined in (1 when none says).
func (t Toolset) Version() int {
	v := 1
	for _, op := range t.Operations {
		v = max(v, op.Since)
	}
	return v
}

// At returns the toolset's operations as of version v.
func (t Toolset) At(v int) []string {
	var out []string
	for _, op := range t.Operations {
		if max(op.Since, 1) <= v {
			out = append(out, op.Name)
		}
	}
	return out
}

// Names returns the toolset's current operations.
func (t Toolset) Names() []string { return t.At(t.Version()) }

var toolsetName = regexp.MustCompile(`^[a-z][a-z0-9_-]*\.[a-z0-9_.-]+$`)

// CheckToolset reports what is wrong with t as the toolset of the
// extension named owner: a name outside its namespace, an operation that
// is neither its own nor a builtin, a repeated operation or a bad version.
func CheckToolset(owner string, t Toolset) error {
	if !toolsetName.MatchString(t.Name) || !strings.HasPrefix(t.Name, owner+".") {
		return fmt.Errorf("toolset %q must be named %s.<name>", t.Name, owner)
	}
	if len(t.Operations) == 0 {
		return fmt.Errorf("toolset %s grants no operations", t.Name)
	}
	seen := map[string]bool{}
	for _, op := range t.Operations {
		if !BuiltinOperation(op.Name) && !strings.HasPrefix(op.Name, owner+".") {
			return fmt.Errorf("toolset %s may grant only %s's own operations and builtins, not %q", t.Name, owner, op.Name)
		}
		if seen[op.Name] {
			return fmt.Errorf("toolset %s lists %s twice", t.Name, op.Name)
		}
		if op.Since < 0 {
			return fmt.Errorf("toolset %s: %s has a negative since", t.Name, op.Name)
		}
		seen[op.Name] = true
	}
	return nil
}

// PlaybookNamer is optionally implemented by an executor to say which
// playbook a binding config makes its turns start on ("" for none), so the
// host can check that the binding grants the operations the playbook
// declares it relies on (PlaybookOperations).
type PlaybookNamer interface {
	Playbook(config json.RawMessage) string
}

// PlaybookOperations are the toolbelt operations a playbook declares it
// relies on, in its frontmatter's operations line: a JSON array, or names
// separated by commas or spaces. A playbook that declares none returns nil.
//
//	---
//	name: work-issue
//	operations: [github.issue.view, github.issue.comment]
//	---
func PlaybookOperations(text string) []string {
	if !strings.HasPrefix(text, "---\n") {
		return nil
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return nil
	}
	for _, line := range strings.Split(text[4:4+end], "\n") {
		v, ok := strings.CutPrefix(line, "operations:")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		var list []string
		if json.Unmarshal([]byte(v), &list) != nil {
			list = strings.FieldsFunc(strings.Trim(v, "[]"), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
		}
		var out []string
		for _, op := range list {
			op = strings.Trim(strings.TrimSpace(op), `"'`)
			if op != "" && !slices.Contains(out, op) {
				out = append(out, op)
			}
		}
		return out
	}
	return nil
}
