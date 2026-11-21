package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// Package provenance (#709): settings.extension_trust, the operator's
// policy on which installed packages may run, enforced where the host
// installs and discovers them (extproc/trust.go has the model). Bundled
// packages are the release's, which the updater verifies, and are not
// subject to it.

// packagePolicy is the policy packages are admitted under: none, a
// policy, or one that couldn't be read, which refuses every installed
// package rather than admit them unchecked.
type packagePolicy struct {
	p   *extproc.Policy
	err error
}

// trustPolicy is the extproc form of settings.extension_trust.
func trustPolicy(t *config.ExtensionTrust) *extproc.Policy {
	if t == nil {
		return nil
	}
	p := &extproc.Policy{Pinned: t.Pinned}
	for _, pub := range t.Publishers {
		p.Publishers = append(p.Publishers, extproc.Publisher{Signer: extproc.Signer{Subject: pub.Subject, SubjectRegexp: pub.SubjectRegexp, Issuer: pub.Issuer}, Packages: pub.Packages})
	}
	return p
}

// filePolicy is the config file's policy. A missing file has none; one
// that can't be read or decoded, or whose policy is malformed, fails
// closed.
func filePolicy(configPath string) packagePolicy {
	if configPath == "" {
		return packagePolicy{}
	}
	data, err := config.ReadFile(configPath)
	if errors.Is(err, fs.ErrNotExist) {
		return packagePolicy{}
	}
	if err != nil {
		return packagePolicy{err: fmt.Errorf("reading %s for settings.extension_trust: %w", configPath, err)}
	}
	return documentPolicy(data)
}

// documentPolicy is a config file's or stored generation's policy. One
// that can't be read fails closed.
func documentPolicy(data []byte) packagePolicy {
	t, err := config.ExtensionTrustOf(data)
	if err != nil {
		return packagePolicy{err: fmt.Errorf("settings.extension_trust can't be read: %w", err)}
	}
	return packagePolicy{p: trustPolicy(t)}
}

// startPolicy is the policy the daemon admits packages under at start: the
// committed generation's, or on a first start the config file's, as the
// package grants are read (startGrants).
func (d *daemon) startPolicy(ctx context.Context) packagePolicy {
	generation, normalized, err := management.StoredGeneration(ctx, d.coord, d.now)
	switch {
	case err != nil:
		return packagePolicy{err: fmt.Errorf("reading the committed configuration for settings.extension_trust: %w", err)}
	case generation != 0:
		return documentPolicy(normalized)
	}
	return filePolicy(d.o.ConfigPath)
}

// admit checks one installed package. The error names the package first,
// as discovery's do.
func (p packagePolicy) admit(f extproc.Found, warn func(name, msg string)) error {
	name := f.Manifest.Name
	if p.err != nil {
		return fmt.Errorf("%s: %w: %w", name, extproc.ErrProvenance, p.err)
	}
	if p.p == nil && warn == nil {
		return nil // nothing to refuse and no one to warn: skip hashing the tree
	}
	pr := extproc.CheckProvenance(f.Dir, name, p.p)
	if pr.Refused != nil {
		return fmt.Errorf("%s: %w", name, pr.Refused)
	}
	if pr.Warning != "" && warn != nil {
		warn(name, pr.Warning)
	}
	return nil
}

// provenanceCheck is doctor's view of the installed packages under a
// policy (the config file's offline, the start policy in the daemon): who
// published each and whether it may run.
func provenanceCheck(configPath string, policy packagePolicy) runtime.Check {
	check := runtime.Check{Name: "extension_trust", Status: runtime.CheckOK, Code: "trusted"}
	installed, _ := extproc.List(extensionsRoot(configPath))
	if len(installed) == 0 {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "not_installed", "no extension packages are installed"
		return check
	}
	switch {
	case policy.err != nil:
		check.Status, check.Code, check.Detail = runtime.CheckFail, "unreadable_policy", policy.err.Error()
		return check
	case policy.p == nil:
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "no_policy", "settings.extension_trust is unset, so every installed package may run, whoever published it"
	default:
		check.Detail = "every installed package is admitted by settings.extension_trust"
	}
	for _, in := range installed {
		name := in.Manifest.Name
		pr := extproc.CheckProvenance(in.Dir, name, policy.p)
		item := fmt.Sprintf("%s %s: %s", name, in.Manifest.Version, pr.Publisher())
		switch {
		case pr.Refused != nil:
			check.Status, check.Code, check.Detail = runtime.CheckFail, "untrusted_package", "settings.extension_trust refuses installed packages; the daemon won't start them"
			item += ", refused: " + pr.Refused.Error()
		case pr.Warning != "":
			if check.Status != runtime.CheckFail {
				check.Status, check.Code = runtime.CheckWarn, "changed_package"
			}
			item += ", " + pr.Warning
		}
		check.Items = append(check.Items, item)
	}
	return check
}

// trustCheck is provenanceCheck for the running daemon, under the policy
// it starts packages with.
func (d *daemon) trustCheck(ctx context.Context) runtime.Check {
	return provenanceCheck(d.o.ConfigPath, d.startPolicy(ctx))
}
