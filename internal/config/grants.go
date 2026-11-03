package config

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// PlaybookNamer is optionally implemented by an executor registration to
// say which playbook a binding config starts its turns on
// (extension.PlaybookNamer).
type PlaybookNamer = extension.PlaybookNamer

// BindingPlaybook is the playbook b's executor starts its turns on with b's
// config, and the operations the playbook declares it relies on. It is ""
// and nil when the executor doesn't say, the binding runs none or the
// playbook declares none.
func BindingPlaybook(reg Registry, b Binding) (string, []string) {
	n, ok := reg.Executors[b.Executor].(PlaybookNamer)
	if !ok {
		return "", nil
	}
	name := n.Playbook(b.Config)
	if name == "" {
		return "", nil
	}
	return name, reg.Playbooks[name]
}

// Declared reports whether operation is one the playbook of the scope's
// binding declares it relies on: a refusal of it is configuration drift,
// the binding or scope not granting what its own playbook uses, rather
// than an agent reaching outside its role.
func (s *Snapshot) Declared(reg Registry, scope domain.ID, binding, operation string) bool {
	e, ok := s.Scopes[scope]
	if !ok {
		return false
	}
	b, ok := e.Binding(binding)
	if !ok {
		return false
	}
	_, ops := BindingPlaybook(reg, b)
	return slices.Contains(ops, operation)
}

// GrantWarnings are what doctor and config check report about operation
// grants that are valid but leave an agent short:
//
//   - a binding whose playbook declares operations the binding or its
//     scope doesn't grant, naming the scope, binding and operations, since
//     its agent would be refused them at run time;
//   - an operations list that names a toolset's operations as of an
//     earlier version instead of the toolset, so it lacks the ones the
//     toolset has gained since; the warning gives the config set command
//     that adopts the toolset.
//
// Disabled scopes are left out.
func GrantWarnings(s *Snapshot, reg Registry) []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, id := range s.Order {
		sc := s.Scopes[id]
		if sc == nil || !sc.Enabled {
			continue
		}
		for _, b := range sc.Bindings {
			playbook, declared := BindingPlaybook(reg, b)
			var scope, binding []string
			for _, op := range declared {
				switch {
				case !slices.Contains(sc.Authorization.Operations, op):
					scope = append(scope, op)
				case !slices.Contains(b.Operations, op):
					binding = append(binding, op)
				}
			}
			if len(scope) > 0 {
				out = append(out, fmt.Sprintf("scope %s binding %s: its playbook %s uses %s, which the scope's authorization.operations doesn't grant; its agent will be refused them. Grant them there and in the binding's operations (or the toolset that has them, as @<name>)",
					sc.ID, b.Name, playbook, strings.Join(scope, ", ")))
			}
			if len(binding) > 0 {
				out = append(out, fmt.Sprintf("scope %s binding %s: its playbook %s uses %s, which the binding's operations don't grant; its agent will be refused them. Add them to the binding's operations (or the toolset that has them, as @<name>)",
					sc.ID, b.Name, playbook, strings.Join(binding, ", ")))
			}
		}
	}
	return append(out, toolsetWarnings(s, reg)...)
}

// toolsetWarnings finds the operations lists, as written, that name an
// earlier version of a toolset's operations one by one.
func toolsetWarnings(s *Snapshot, reg Registry) []string {
	doc, err := Parse(s.Normalized)
	if err != nil || len(reg.Toolsets) == 0 {
		return nil
	}
	names := make([]string, 0, len(reg.Toolsets))
	for name := range reg.Toolsets {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []string
	check := func(where, pointer string, list []string) {
		for _, name := range names {
			t := reg.Toolsets[name]
			missing, v := behind(t, list)
			if len(missing) == 0 {
				continue
			}
			adopted := []string{}
			for _, entry := range list {
				if !slices.Contains(t.Names(), entry) {
					adopted = append(adopted, entry)
				}
			}
			adopted = append(adopted, "@"+t.Name)
			value, _ := json.Marshal(adopted)
			out = append(out, fmt.Sprintf("%s lists the %s toolset's operations as of its version %d, so it lacks %s, which the toolset has gained since. Grant the toolset instead, so it gets what the toolset gains: open-conduit config set %s '%s'",
				where, t.Name, v, strings.Join(missing, ", "), pointer, value))
		}
	}
	for _, sc := range doc.Scopes {
		if e := s.Scopes[domain.ID(sc.ID)]; e == nil || !e.Enabled {
			continue
		}
		if a := sc.Authorization; a != nil {
			check("scope "+sc.ID+" authorization.operations", "/scopes/"+sc.ID+"/authorization/operations", a.Operations)
		}
		for _, b := range sc.Bindings {
			if b.Disabled {
				continue
			}
			check("scope "+sc.ID+" binding "+b.Name+" operations", "/scopes/"+sc.ID+"/bindings/"+b.Name+"/operations", b.Operations)
		}
	}
	return out
}

// behind reports the operations a list lacks when it names every operation
// of an earlier version of t, but not t itself, and that version. It is
// nil when the list names t (or denies it), names no earlier version whole,
// or lacks nothing except what it denies.
func behind(t extension.Toolset, list []string) ([]string, int) {
	if slices.Contains(list, "@"+t.Name) || slices.Contains(list, "!@"+t.Name) {
		return nil, 0
	}
	for v := t.Version() - 1; v >= 1; v-- {
		if !all(t.At(v), func(op string) bool { return slices.Contains(list, op) }) {
			continue
		}
		var missing []string
		for _, op := range t.Names() {
			if !slices.Contains(list, op) && !slices.Contains(list, "!"+op) {
				missing = append(missing, op)
			}
		}
		return missing, v
	}
	return nil, 0
}
