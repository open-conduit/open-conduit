package config

import (
	"fmt"
	"maps"
	"slices"

	"github.com/open-conduit/open-conduit/domain"
)

// PackageWarnings are the settings.extensions entries that name no
// installed or bundled package (Registry.Packages). They are warnings, not
// errors, so a configuration survives removing a package; the grant takes
// effect again if the package is reinstalled.
func PackageWarnings(s *Snapshot, reg Registry) []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, name := range slices.Sorted(maps.Keys(s.Settings.Extensions)) {
		if !reg.Packages[name] {
			out = append(out, fmt.Sprintf("settings.extensions.%s names no installed or bundled extension package, so its grant reaches nothing; install the package or remove the entry", name))
		}
	}
	return out
}

// PackageGrants are the variables settings.extensions grants each package
// by name, leaving out any name validation refuses (pass_env_reserved,
// pass_env_secret). A document read before it is validated (a config file
// the daemon starts its packages from, or one being checked) therefore
// never passes one validation would refuse.
func (d *Document) PackageGrants() map[string][]string {
	refs := envSecretNames(d)
	out := map[string][]string{}
	for name, ps := range d.Settings.Extensions {
		for _, v := range ps.PassEnv {
			if !domain.ReservedEnv(v) && !refs[v] {
				out[name] = append(out[name], v)
			}
		}
	}
	return out
}

// FileGrants are PackageGrants of the config file data, or none when it
// doesn't parse.
func FileGrants(data []byte) map[string][]string {
	doc, err := ParseFile(data)
	if err != nil {
		return nil
	}
	return doc.PackageGrants()
}

// ExtensionTrustOf reads settings.extension_trust alone from a config file
// or stored generation, so that a problem elsewhere in the document never
// drops the operator's package policy. It is nil when the document sets
// none, and an error when the document can't be decoded or the policy is
// malformed: the caller fails closed.
func ExtensionTrustOf(data []byte) (*ExtensionTrust, error) {
	root, diags := decode(data)
	if root == nil {
		return nil, invalid(diags)
	}
	c := &checker{}
	t := c.extensionTrust(root.field("settings").field("extension_trust"))
	if len(c.diags) > 0 {
		return nil, invalid(c.diags)
	}
	return t, nil
}
