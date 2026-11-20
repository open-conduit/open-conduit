package config

import (
	"encoding/json"
	"strings"
)

// ExportFile turns a stored generation's normalized document back into
// config file text: the file's own scopes, with each included folder's
// scopes replaced by the include that brought them in (its folder and the
// file scope they sit under), as ParseFile reads it. Secret references stay
// reference text, never values. A folder's config is not written: its
// scopes come from the folder when the file is applied, as before. A
// submitted include is written as its name and parent: its scopes stay in
// the daemon, carried over by the next apply, and FragmentOf reads them.
func ExportFile(stored []byte) ([]byte, error) {
	doc, err := Parse(stored)
	if err != nil {
		return nil, err
	}
	file := *doc
	file.Scopes, file.Includes = nil, nil
	from := map[string]string{}
	for _, sc := range doc.Scopes {
		if sc.Source == "" {
			file.Scopes = append(file.Scopes, sc)
		} else {
			from[sc.ID] = sc.Source
		}
	}
	seen := map[string]bool{}
	for _, sc := range doc.Scopes {
		// A folder's top scope is the one whose parent isn't from that
		// folder: its parent is the include's.
		if sc.Source == "" || strings.HasPrefix(sc.Source, "submitted:") || seen[sc.Source] || from[sc.Parent] == sc.Source {
			continue
		}
		seen[sc.Source] = true
		file.Includes = append(file.Includes, Include{Path: sc.Source, Parent: sc.Parent})
	}
	file.Includes = append(file.Includes, doc.Submitted()...)
	m := file.normalized(false).(map[string]any)
	delete(m, "includes")
	if len(file.Includes) > 0 {
		list := []any{}
		for _, in := range file.Includes {
			if in.Name != "" {
				list = append(list, in.normalized())
			} else {
				list = append(list, map[string]any{"path": in.Path, "parent": in.Parent})
			}
		}
		m["includes"] = list
	}
	out, err := json.MarshalIndent(canonicalValue(m), "", "  ")
	if err != nil {
		return nil, err
	}
	if _, err = ParseFile(out); err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// canonicalValue is v as canonical renders it, as plain JSON values, so it
// can be indented.
func canonicalValue(v any) any {
	var plain any
	if err := json.Unmarshal(canonical(v), &plain); err != nil {
		return v
	}
	return plain
}
