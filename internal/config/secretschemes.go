package config

import (
	"maps"
	"slices"
)

// secretSchemes checks every provider reference against the packages: its
// scheme is one an installed or bundled package declares, and no provider
// package's own credentials are resolved, directly or through other
// providers, by a scheme it serves. Diagnostics never echo the reference.
func (v *validator) secretSchemes() {
	check := func(path string, refs map[string]string) {
		for _, name := range slices.Sorted(maps.Keys(refs)) {
			scheme := ProviderScheme(refs[name])
			if scheme == "" {
				continue
			}
			if _, ok := v.reg.SecretSchemes[scheme]; !ok {
				v.fail(ptr(path, name), "unknown_registration", "no installed or bundled package provides this secret reference's scheme; install one, or use keyring:, file:, env: or cmd:"+registeredNames(v.reg.SecretSchemes, scheme))
			}
		}
	}
	for i, c := range v.doc.Connections {
		check(ptr("connections", i)+"/secret_refs", c.SecretRefs)
	}
	for i, s := range v.doc.Scopes {
		check(ptr("scopes", i)+"/secret_refs", s.SecretRefs)
	}
	v.providerCycles()
}

// providerEdge is a provider package's credential that another (or the
// same) provider package resolves, with where it is configured.
type providerEdge struct{ to, path string }

// providerCycles refuses a provider package whose own credentials (the
// secrets of connections of its types, and the scope secrets its entries
// select) resolve through a scheme it serves, directly or through a chain
// of other providers: the host would have to ask the package for the
// credential it needs to answer.
func (v *validator) providerCycles() {
	providers := map[string]bool{}
	for _, pkg := range v.reg.SecretSchemes {
		providers[pkg] = true
	}
	if len(providers) == 0 {
		return
	}
	edges := map[string][]providerEdge{}
	add := func(from, path, ref string) {
		if !providers[from] {
			return
		}
		if to, ok := v.reg.SecretSchemes[ProviderScheme(ref)]; ok {
			edges[from] = append(edges[from], providerEdge{to, path})
		}
	}
	connIndex := map[string]int{}
	for i, c := range v.doc.Connections {
		connIndex[c.ID] = i
		owner := v.reg.ConnectionOwners[c.Type]
		for _, name := range slices.Sorted(maps.Keys(c.SecretRefs)) {
			add(owner, ptr(ptr("connections", i)+"/secret_refs", name), c.SecretRefs[name])
		}
	}
	for i, sc := range v.doc.Scopes {
		e := v.state[sc.ID]
		if e == nil {
			continue
		}
		for _, x := range e.extensions.active() {
			owner := v.reg.Extensions[x.Type].Owner
			for _, name := range x.SecretNames {
				add(owner, ptr("scopes", i)+"/extensions", e.refs[name])
			}
			if j, ok := connIndex[x.Connection]; ok {
				c := v.doc.Connections[j]
				for _, name := range slices.Sorted(maps.Keys(c.SecretRefs)) {
					add(owner, ptr(ptr("connections", j)+"/secret_refs", name), c.SecretRefs[name])
				}
			}
		}
	}
	reported := map[string]bool{}
	for _, start := range slices.Sorted(maps.Keys(providers)) {
		// A breadth-first walk from start's credentials: reaching start
		// again is a cycle, reported at the first credential on it.
		seen := map[string]bool{}
		type step struct{ pkg, path string }
		var queue []step
		for _, e := range edges[start] {
			queue = append(queue, step{e.to, e.path})
		}
		for len(queue) > 0 {
			s := queue[0]
			queue = queue[1:]
			if s.pkg == start {
				if !reported[s.path] {
					reported[s.path] = true
					v.fail(s.path, "provider_cycle", "the "+start+" package serves the secret scheme this credential of its own resolves through, directly or through other providers; give it its identity through settings.extensions."+start+".pass_env, or an env: or file: reference")
				}
				break
			}
			if seen[s.pkg] {
				continue
			}
			seen[s.pkg] = true
			for _, e := range edges[s.pkg] {
				queue = append(queue, step{e.to, s.path})
			}
		}
	}
}
