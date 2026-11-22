package config

import (
	"encoding/json"
	"errors"
	"strings"
)

// ErrUnknownInclude is a submitted include the committed generation does
// not declare.
var ErrUnknownInclude = errors.New("no submitted include of that name")

// Fragment is a submitted include as its team reads it: the scope it sits
// under, its fragment's text as ApplyFragment takes it back, and the
// fragment's hash, which an apply names as the one it replaces.
type Fragment struct {
	Name, Parent string
	Text         []byte
	Hash         string
}

// FragmentOf reads the submitted include name from a stored generation.
// An include nothing has been applied to yet has no scopes.
func FragmentOf(stored []byte, name string) (Fragment, error) {
	doc, err := Parse(stored)
	if err != nil {
		return Fragment{}, err
	}
	in, ok := submittedInclude(doc, name)
	if !ok {
		return Fragment{}, ErrUnknownInclude
	}
	scopes := submittedScopes(doc, in)
	body := fragmentValue(scopes, in.Parent)
	text, err := json.MarshalIndent(canonicalValue(body), "", "  ")
	if err != nil {
		return Fragment{}, err
	}
	return Fragment{Name: name, Parent: in.Parent, Text: append(text, '\n'), Hash: hashOf(canonical(body))}, nil
}

// fragmentValue is scopes as a fragment holds them: without the source the
// generation records, and with a top scope's parent (the include's) left
// out.
func fragmentValue(scopes []ScopeConfig, parent string) map[string]any {
	list := []any{}
	for _, sc := range scopes {
		sc.Source = ""
		if sc.Parent == parent {
			sc.Parent = ""
		}
		list = append(list, (&Document{Scopes: []ScopeConfig{sc}}).normalized(false).(map[string]any)["scopes"].([]any)[0])
	}
	return map[string]any{"version": 1, "scopes": list}
}

func submittedInclude(doc *Document, name string) (Include, bool) {
	for _, in := range doc.Includes {
		if in.Name == name {
			return in, true
		}
	}
	return Include{}, false
}

// ApplyFragment composes a fragment submitted for the include name with
// the stored generation it replaces the include's scopes in, and validates
// the result: the same full validation a config file's apply gets, against
// the committed configuration rather than the file on disk, so no folder
// is read and nothing else changes. The fragment is what a folder holds,
// { "version": 1, "scopes": [...] }, under the same rules: no secret
// references or pass_env, and every scope beneath the include's parent.
// Its bindings are stored as given, since there is no folder to adapt
// them to.
//
// A diagnostic says which side owns the problem: one in the fragment is at
// fragment/..., as the fragment's own path; one anywhere else, which the
// fragment can still cause (by clashing with a scope ID, say), is at
// config/..., its path in the committed configuration.
func ApplyFragment(stored []byte, name string, fragment []byte, reg Registry) (*Snapshot, error) {
	doc, err := Parse(stored)
	if err != nil {
		return nil, err
	}
	in, ok := submittedInclude(doc, name)
	if !ok {
		return nil, ErrUnknownInclude
	}
	c := &checker{fragment: true}
	scopes := c.fragmentScopes(fragment, "fragment", in.Parent)
	if len(c.diags) > 0 {
		return nil, invalid(c.diags)
	}
	// Scopes not from a submitted include keep their order; the submitted
	// ones follow in include order, as Compose places them.
	var composed []ScopeConfig
	var origins []string
	for _, sc := range doc.Scopes {
		if !strings.HasPrefix(sc.Source, "submitted:") {
			composed = append(composed, sc)
			origins = append(origins, "")
		}
	}
	for _, other := range doc.Includes {
		if other.Name != name {
			for _, sc := range submittedScopes(doc, other) {
				composed = append(composed, sc)
				origins = append(origins, "")
			}
			continue
		}
		for j, sc := range scopes {
			sc.Source = SubmittedSource(name)
			composed = append(composed, sc)
			origins = append(origins, ptr("fragment/scopes", j))
		}
	}
	doc.Scopes = composed
	s, err := validateDocument(doc, reg, policyMode{fragment: name})
	var invalidErr *ValidationError
	if errors.As(err, &invalidErr) {
		for i, d := range invalidErr.Diagnostics {
			if p := remap(d.Path, origins); strings.HasPrefix(p, "fragment/") {
				invalidErr.Diagnostics[i].Path = p
			} else {
				invalidErr.Diagnostics[i].Path = strings.TrimSuffix("config/"+d.Path, "/")
			}
		}
		return nil, invalid(invalidErr.Diagnostics)
	}
	return s, err
}
