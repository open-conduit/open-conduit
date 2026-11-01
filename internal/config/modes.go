package config

import (
	"bytes"
	"fmt"
)

// ModeWarnings returns one warning per binding whose resource config names a
// default mode that never applies, because the binding's executor requests a
// different mode of its own with that config (the executor's request wins).
// A binding a scope inherits unchanged is reported once, at the scope that
// declares it.
func ModeWarnings(s *Snapshot, reg Registry) []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, id := range s.Order {
		sc := s.Scopes[id]
		if sc == nil {
			continue
		}
		for _, b := range sc.Bindings {
			if b.Resource == nil || inherited(s, sc, b) {
				continue
			}
			def := reg.ResourceModeDefault[b.Resource.Provider]
			moder, ok := reg.Executors[b.Executor].(ResourceModer)
			if def == nil || !ok {
				continue
			}
			set, requested := def(b.Resource.Config), moder.ResourceMode(b.Config)
			if set == "" || requested == "" || set == requested {
				continue
			}
			out = append(out, fmt.Sprintf("scope %s binding %s: the %s resource's mode %q never applies, because executor %s requests mode %q with this config; remove the resource's mode, or set the mode in the executor's config",
				sc.ID, b.Name, b.Resource.Provider, set, b.Executor, requested))
		}
	}
	return out
}

// inherited reports whether b is the same binding sc's parent has, so its
// warning belongs to the parent.
func inherited(s *Snapshot, sc *EffectiveScope, b Binding) bool {
	parent := s.Scopes[sc.ParentID]
	if sc.ParentID == "" || parent == nil {
		return false
	}
	for _, pb := range parent.Bindings {
		if pb.Name == b.Name {
			return pb.Executor == b.Executor && pb.Resource != nil && pb.Resource.Provider == b.Resource.Provider &&
				bytes.Equal(pb.Resource.Config, b.Resource.Config) && bytes.Equal(pb.Config, b.Config)
		}
	}
	return false
}
