package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/open-conduit/open-conduit/domain"
)

// PolicyDenied is the diagnostic and secret failure code of something the
// daemon's policy refuses.
const PolicyDenied = "policy_denied"

// ErrUnsafePolicy rejects a policy file that isn't a regular file, or that
// someone other than its owner can write.
var ErrUnsafePolicy = errors.New("unsafe policy file")

// PolicyLimits are ceilings on the config's limits; zero sets none.
type PolicyLimits struct {
	Concurrency, JobConcurrency, ScopeConcurrency, AdmissionLimit int
}

// Policy is the operator's layer above the config: supplied when the daemon
// starts (--policy, OPEN_CONDUIT_POLICY), read once, and written by no
// management route. Every config must fit inside it, as a child scope's
// authorization fits inside its parent's. A nil *Policy allows everything,
// and every method is safe on one. A nil list leaves that thing
// unrestricted; an empty one allows none of it.
type Policy struct {
	// Path and Digest (SHA-256 of the file) identify what was loaded.
	Path, Digest string
	// WritableByDaemon says the daemon's own user could change what the path
	// reads, by writing the file or replacing it or a directory or symlink
	// on the way to it, so it binds a config author but not code running as
	// that user.
	WritableByDaemon bool

	SecretSchemes, Executors, EntryTypes, PassEnv []string
	// NoIncludedFolders refuses includes[].path. Submitted includes are
	// bounded by their declaration and stay allowed.
	NoIncludedFolders bool
	Limits            PolicyLimits
	// Budgets hold whatever the config says: it can't remove them or raise
	// them, and an operator's release doesn't pass them.
	Budgets []Budget
	// Isolation is IsolationRequired to hold authorization.isolation at
	// every root scope.
	Isolation string
	// ExtensionTrust must admit an installed package, as well as any
	// settings.extension_trust the config sets.
	ExtensionTrust *ExtensionTrust
}

// LoadPolicy reads the policy file at path, following symlinks (a mounted
// ConfigMap is one). Unlike the daemon's private files it may belong to
// another user and be world-readable, but only its owner may write it. An
// empty path is no policy.
func LoadPolicy(path string) (*Policy, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrUnsafePolicy, path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%w: %s is writable by its group or others", ErrUnsafePolicy, path)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSize {
		return nil, fmt.Errorf("%w: %s is larger than 1 MiB", ErrUnsafePolicy, path)
	}
	p, err := ParsePolicy(data)
	if err != nil {
		return nil, err
	}
	p.Path = path
	p.WritableByDaemon = replaceableByDaemon(path)
	return p, nil
}

const (
	accessWrite      = 2 // W_OK
	accessWriteEnter = 3 // W_OK|X_OK, what renaming an entry of a directory needs
)

// replaceableByDaemon reports whether this process could change what path
// reads at the next start: by writing the file, or by replacing the file, a
// symlink on the way to it, or a directory above any of them.
func replaceableByDaemon(path string) bool {
	if syscall.Access(path, accessWrite) == nil {
		return true
	}
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(p string) bool {
		p = filepath.Clean(p)
		if seen[p] {
			return false
		}
		seen[p] = true
		info, err := os.Lstat(p)
		if err != nil {
			return true // can't tell, so don't report it safe
		}
		if parent := filepath.Dir(p); parent != p {
			if entryReplaceable(parent, info) || walk(parent) {
				return true
			}
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return true
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(p), target)
			}
			return walk(target)
		}
		return false
	}
	abs, err := filepath.Abs(path)
	return err != nil || walk(abs)
}

// entryReplaceable reports whether this process can rename or remove the
// entry of dir described by entry.
func entryReplaceable(dir string, entry os.FileInfo) bool {
	if syscall.Access(dir, accessWriteEnter) != nil {
		return false
	}
	info, err := os.Stat(dir)
	if err != nil {
		return true
	}
	return !stickyProtects(info.Mode()&os.ModeSticky != 0, ownerOf(info), ownerOf(entry), uint32(os.Getuid()))
}

// stickyProtects reports whether a sticky directory stops uid replacing an
// entry: only the entry's owner, the directory's owner or root may.
func stickyProtects(sticky bool, dirOwner, entryOwner, uid uint32) bool {
	return sticky && uid != 0 && uid != dirOwner && uid != entryOwner
}

func ownerOf(info os.FileInfo) uint32 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Uid
	}
	return uint32(os.Getuid()) // unknown counts as ours, so it isn't reported safe
}

// ParsePolicy strictly decodes a policy document: an unknown field, a
// duplicate key or a bad value is an error, so a typo can't loosen it.
func ParsePolicy(data []byte) (*Policy, error) {
	root, diags := decode(data)
	if root == nil {
		return nil, invalid(diags)
	}
	c := &checker{diags: diags}
	p := &Policy{}
	sum := sha256.Sum256(data)
	p.Digest = hex.EncodeToString(sum[:])
	if c.object(root, "", []string{"version"}, "secret_schemes", "executors", "entry_types", "pass_env", "included_folders", "limits", "budgets", "isolation", "extension_trust") {
		if v := root.field("version"); v != nil && c.typed(v, "version", kNumber, "integer") && v.num.String() != "1" {
			c.fail("version", "unsupported_version", "version must be exactly 1")
		}
		for _, l := range []struct {
			key string
			dst *[]string
		}{{"secret_schemes", &p.SecretSchemes}, {"executors", &p.Executors}, {"entry_types", &p.EntryTypes}, {"pass_env", &p.PassEnv}} {
			if n := root.field(l.key); n != nil {
				*l.dst = c.list(n, l.key, nil)
			}
		}
		if n := root.field("included_folders"); n != nil && c.typed(n, "included_folders", kBool, "boolean") {
			p.NoIncludedFolders = !n.b
		}
		if n := root.field("limits"); n != nil && c.object(n, "limits", nil, "concurrency", "job_concurrency", "scope_concurrency", "admission_limit") {
			p.Limits.Concurrency, _ = c.integer(n.field("concurrency"), "limits/concurrency")
			p.Limits.JobConcurrency, _ = c.integer(n.field("job_concurrency"), "limits/job_concurrency")
			p.Limits.ScopeConcurrency, _ = c.integer(n.field("scope_concurrency"), "limits/scope_concurrency")
			p.Limits.AdmissionLimit, _ = c.integer(n.field("admission_limit"), "limits/admission_limit")
		}
		p.Budgets = c.budgets(root.field("budgets"))
		if n := root.field("isolation"); n != nil {
			p.Isolation, _ = c.enum(n, "isolation", IsolationRequired)
		}
		p.ExtensionTrust = c.extensionTrust(root.field("extension_trust"))
	}
	if len(c.diags) > 0 {
		// The budget and trust checks report the paths they have in a config.
		for i, d := range c.diags {
			c.diags[i].Path = strings.TrimPrefix(d.Path, "settings/")
		}
		return nil, invalid(c.diags)
	}
	return p, nil
}

func allowedBy(list []string, v string) bool { return list == nil || slices.Contains(list, v) }

// AllowsScheme reports whether a secret reference may use scheme.
func (p *Policy) AllowsScheme(scheme string) bool {
	return p == nil || allowedBy(p.SecretSchemes, scheme)
}

// AllowsPassEnv reports whether pass_env may name the daemon variable.
func (p *Policy) AllowsPassEnv(name string) bool {
	return p == nil || allowedBy(p.PassEnv, name)
}

// RequiresIsolation reports whether the policy holds isolation at the root.
func (p *Policy) RequiresIsolation() bool { return p != nil && p.Isolation == IsolationRequired }

// RefusesIncludedFolders reports whether includes[].path is refused.
func (p *Policy) RefusesIncludedFolders() bool { return p != nil && p.NoIncludedFolders }

// FilterGrants is grants without the variables the policy refuses, for
// packages that start before a generation is validated.
func (p *Policy) FilterGrants(grants map[string][]string) map[string][]string {
	if p == nil || p.PassEnv == nil || grants == nil {
		return grants
	}
	out := make(map[string][]string, len(grants))
	for name, vars := range grants {
		out[name] = slices.DeleteFunc(slices.Clone(vars), func(v string) bool { return !p.AllowsPassEnv(v) })
	}
	return out
}

// policyMode says which ceilings a validation refuses rather than clamps.
// Every other rule is refused in every mode.
type policyMode struct {
	// stored validates a committed generation: ceilings clamp it, so that
	// lowering one doesn't stop a daemon.
	stored bool
	// fragment is the submitted include being applied: only its own scopes
	// answer for their limits, since its author can't change the rest.
	fragment string
}

// refusesScopeLimits reports whether a scope from source answers for its
// limits. A submitted include's scopes answer only to their own apply, so a
// lowered ceiling can't block a root apply on a team's fragment.
func (m policyMode) refusesScopeLimits(source string) bool {
	switch {
	case m.stored:
		return false
	case m.fragment != "":
		return source == SubmittedSource(m.fragment)
	}
	return !strings.HasPrefix(source, "submitted:")
}

func (m policyMode) refusesSettings() bool { return !m.stored && m.fragment == "" }

// ValidateStored validates a committed generation: as Validate, except that
// the policy's ceilings clamp it instead of refusing it.
func ValidateStored(data []byte, reg Registry) (*Snapshot, error) {
	doc, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return validateDocument(doc, reg, policyMode{stored: true})
}

// PolicyRefused reports whether err is a validation failure with at least
// one policy_denied diagnostic.
func PolicyRefused(err error) bool {
	var invalid *ValidationError
	return errors.As(err, &invalid) && slices.ContainsFunc(invalid.Diagnostics, func(d Diagnostic) bool { return d.Code == PolicyDenied })
}

// policy reports one policy_denied diagnostic per offending pointer. It
// never echoes a secret reference.
func (v *validator) policy() {
	p := v.reg.Policy
	if p == nil {
		return
	}
	deny := func(path, msg string) { v.fail(path, PolicyDenied, "the daemon's policy "+msg) }
	refs := func(path string, set map[string]string) {
		for name, ref := range set {
			if ref == "" {
				continue
			}
			if scheme, _, _ := strings.Cut(ref, ":"); !p.AllowsScheme(scheme) {
				deny(ptr(path, name), "does not allow the secret scheme "+scheme+"; use one of: "+listed(p.SecretSchemes))
			}
		}
	}
	single := func(path, ref string) {
		if scheme, _, ok := strings.Cut(ref, ":"); ok && !p.AllowsScheme(scheme) {
			deny(path, "does not allow the secret scheme "+scheme+"; use one of: "+listed(p.SecretSchemes))
		}
	}
	for i, c := range v.doc.Connections {
		refs(ptr("connections", i)+"/secret_refs", c.SecretRefs)
	}
	single("settings/backup/signing_key", v.doc.Settings.Backup.SigningKey)
	if f := v.doc.Settings.Webhooks.Forwarder; f != nil {
		single("settings/webhooks/forwarder/secret", f.Secret)
	}
	for i, sc := range v.doc.Scopes {
		path := ptr("scopes", i)
		refs(path+"/secret_refs", sc.SecretRefs)
		if p.NoIncludedFolders && sc.Source != "" && !strings.HasPrefix(sc.Source, "submitted:") {
			deny(path, "does not allow included folders, and this scope came from one")
		}
		for j, b := range sc.Bindings {
			if !b.Disabled && !allowedBy(p.Executors, b.Executor) {
				deny(ptr(path+"/bindings", j)+"/executor", "does not allow the executor "+b.Executor+"; use one of: "+listed(p.Executors))
			}
		}
		for j, x := range sc.Extensions {
			if !x.Disabled && !allowedBy(p.EntryTypes, x.Type) {
				deny(ptr(path+"/extensions", j)+"/type", "does not allow the entry type "+x.Type+"; use one of: "+listed(p.EntryTypes))
			}
		}
		if v.mode.refusesScopeLimits(sc.Source) {
			if c := smallest(p.Limits.Concurrency, p.Limits.ScopeConcurrency); c > 0 && sc.Limits.Concurrency > c {
				deny(path+"/limits/concurrency", fmt.Sprintf("caps scope concurrency at %d", c))
			}
			if c := p.Limits.AdmissionLimit; c > 0 && sc.Limits.AdmissionLimit > c {
				deny(path+"/limits/admission_limit", fmt.Sprintf("caps admission_limit at %d", c))
			}
		}
	}
	if !v.mode.refusesSettings() {
		return
	}
	s := v.doc.Settings
	// A default the author never wrote takes the ceiling instead.
	if c := p.Limits.Concurrency; c > 0 && v.doc.written["concurrency"] && s.Concurrency > c {
		deny("settings/concurrency", fmt.Sprintf("caps settings.concurrency at %d", c))
	}
	if c := p.Limits.JobConcurrency; c > 0 && v.doc.written["job_concurrency"] && s.JobConcurrency > c {
		deny("settings/job_concurrency", fmt.Sprintf("caps settings.job_concurrency at %d", c))
	}
	if c := p.Limits.AdmissionLimit; c > 0 && s.AdmissionLimit > c {
		deny("settings/admission_limit", fmt.Sprintf("caps settings.admission_limit at %d", c))
	}
	for i, b := range s.Budgets {
		for _, pinned := range p.Budgets {
			if sameBudget(b, pinned) && b.Max > pinned.Max {
				deny(ptr("settings/budgets", i)+"/max", fmt.Sprintf("caps this budget at %g", pinned.Max))
			}
		}
	}
}

func listed(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// smallest is the lower of two ceilings, where zero sets none.
func smallest(a, b int) int {
	if a == 0 || (b > 0 && b < a) {
		return b
	}
	return a
}

// sameBudget reports whether two budgets count one ledger.
func sameBudget(a, b Budget) bool {
	return a.Unit == b.Unit && a.Per == b.Per && a.Period == b.Period && a.Scope == b.Scope
}

// clamp makes the snapshot's effective limits fit the policy and adds its
// budgets. The stored document and hash are untouched, so raising a ceiling
// again restores what the config says. PolicyClamps says what changed.
func (p *Policy) clamp(s *Snapshot) {
	if p == nil {
		return
	}
	limit := func(name string, v *int, c int) {
		if c > 0 && *v > c {
			s.PolicyClamps = append(s.PolicyClamps, fmt.Sprintf("%s %d runs at the policy's ceiling of %d", name, *v, c))
			*v = c
		}
	}
	settings := [2]int{s.Settings.Concurrency, s.Settings.AdmissionLimit}
	limit("settings.concurrency", &s.Settings.Concurrency, p.Limits.Concurrency)
	limit("settings.job_concurrency", &s.Settings.JobConcurrency, p.Limits.JobConcurrency)
	limit("settings.admission_limit", &s.Settings.AdmissionLimit, p.Limits.AdmissionLimit)
	before := make(map[domain.ID][2]int, len(s.Order))
	for _, id := range s.Order {
		before[id] = [2]int{s.Scopes[id].Concurrency, s.Scopes[id].AdmissionLimit}
	}
	concurrency := smallest(p.Limits.Concurrency, p.Limits.ScopeConcurrency)
	for _, id := range s.Order {
		sc := s.Scopes[id]
		parent, nested := before[sc.ParentID]
		// A value inherited from something already reported is not reported
		// again.
		reported := func(i, ceiling, settingsCeiling int) bool {
			if nested {
				return parent[i] == before[id][i]
			}
			return settings[i] == before[id][i] && settingsCeiling == ceiling
		}
		if c := concurrency; c > 0 && sc.Concurrency > c {
			if reported(0, c, p.Limits.Concurrency) {
				sc.Concurrency = c
			} else {
				limit("scope "+string(id)+" concurrency", &sc.Concurrency, c)
			}
		}
		if c := p.Limits.AdmissionLimit; c > 0 {
			switch {
			case sc.AdmissionLimit == 0:
				sc.AdmissionLimit = c // no cap of its own: the ceiling is one
			case sc.AdmissionLimit > c && reported(1, c, c):
				sc.AdmissionLimit = c
			default:
				limit("scope "+string(id)+" admission_limit", &sc.AdmissionLimit, c)
			}
		}
	}
	for _, pinned := range p.Budgets {
		pinned.PinnedMax = pinned.Max
		i := slices.IndexFunc(s.Settings.Budgets, func(b Budget) bool { return sameBudget(b, pinned) })
		if i < 0 {
			s.Settings.Budgets = append(s.Settings.Budgets, pinned)
			continue
		}
		b := &s.Settings.Budgets[i]
		if b.Max > pinned.Max {
			s.PolicyClamps = append(s.PolicyClamps, fmt.Sprintf("a %s budget of %g runs at the policy's ceiling of %g", b.Unit, b.Max, pinned.Max))
			b.Max = pinned.Max
		}
		b.PinnedMax = pinned.Max
	}
}
