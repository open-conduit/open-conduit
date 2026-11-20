package extproc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Package provenance (#709). An installed package runs with the
// operator's privileges, so which packages may run is the operator's to
// say: settings.extension_trust (a Policy) names the publishers they trust,
// each as a Sigstore identity, and the package digests they pin. Without a
// policy, any package installs and runs, as before.
//
// Install records what it installed in the package directory (RecordFile):
// a digest of the whole package tree, the archive's digest, where it came
// from and, when a signature bundle verified, the signer's identity. With a
// policy, install refuses a package that no trusted publisher signed and no
// pin names, and discovery at start refuses one whose tree no longer matches
// its record or whose record the current policy no longer admits. Start
// needs no network: the signature is verified once, at install, and start
// compares the record. A process running as the operator's user can rewrite
// a package and its record alike; the record proves what was installed, not
// that nothing changed it since.

// RecordFile is the install record in an installed package's directory. It
// is the installer's: never part of the tree digest, and never copied from
// a package's source.
const RecordFile = ".open-conduit-install.json"

// SignatureSuffix is appended to an archive's location to find the
// Sigstore bundle published beside it (cosign sign-blob --bundle).
const SignatureSuffix = ".sigstore.json"

// Signer is a Sigstore identity: a certificate subject (exact, or a
// regular expression matching the whole subject) and its OIDC issuer.
type Signer struct {
	Subject       string `json:"subject,omitempty"`
	SubjectRegexp string `json:"subject_regexp,omitempty"`
	Issuer        string `json:"issuer"`
}

func (s Signer) String() string {
	subject := s.Subject
	if subject == "" {
		subject = "/" + s.SubjectRegexp + "/"
	}
	return subject + " (issuer " + s.Issuer + ")"
}

// matches reports whether a verified signer (an exact subject) is this
// identity.
func (s Signer) matches(got Signer) bool {
	if got.Issuer != s.Issuer || got.Subject == "" {
		return false
	}
	if s.SubjectRegexp == "" {
		return got.Subject == s.Subject
	}
	re, err := regexp.Compile("^(?:" + s.SubjectRegexp + ")$")
	return err == nil && re.MatchString(got.Subject)
}

// Publisher is one trusted publisher: its identity and, when set, the only
// packages it is trusted for.
type Publisher struct {
	Signer
	Packages []string
}

func (p Publisher) covers(name string) bool {
	return len(p.Packages) == 0 || slices.Contains(p.Packages, name)
}

// Policy is the operator's package provenance policy
// (settings.extension_trust). A nil Policy admits every package.
type Policy struct {
	Publishers []Publisher
	// Pinned are the digests admitted for each package by name: the
	// package tree's (Package.Digest) or its release archive's SHA-256,
	// as lower-case hex.
	Pinned map[string][]string
}

// SignatureCheck verifies that bundle is a Sigstore signature over
// artifact by the identity want, and returns the identity that signed it
// (its exact certificate subject and issuer). The open-conduit entrypoint
// supplies it; nil means this build cannot verify signatures.
type SignatureCheck func(ctx context.Context, bundle, artifact []byte, want Signer) (Signer, error)

// Record is what Install recorded about an installed package.
type Record struct {
	// Tree is the package tree's digest at install (Package.Digest).
	Tree string `json:"tree"`
	// Archive is the release archive's SHA-256 ("" for a directory).
	Archive string `json:"archive,omitempty"`
	// Source is the directory, path or URL it was installed from.
	Source string `json:"source"`
	// Signer is the identity a signature bundle verified against, nil
	// when none was checked.
	Signer *Signer `json:"signer,omitempty"`
	// Pinned is set when a pinned digest admitted the package.
	Pinned      bool      `json:"pinned,omitempty"`
	InstalledAt time.Time `json:"installed_at"`
}

// ErrProvenance wraps every package the provenance policy refuses, at
// install or at start.
var ErrProvenance = errors.New("extproc: the package's provenance is not trusted")

// TreeDigest is the SHA-256 of a package directory's whole tree: every
// regular file's relative path, whether it is executable, and its content,
// in path order. The install record is left out.
func TreeDigest(dir string) (string, error) {
	type entry struct {
		rel  string
		exec bool
		sum  [sha256.Size]byte
	}
	var entries []entry
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == RecordFile {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", ErrPackage, rel)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		e := entry{rel: rel, exec: info.Mode().Perm()&0o111 != 0}
		copy(e.sum[:], h.Sum(nil))
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	for _, e := range entries {
		mode := "-"
		if e.exec {
			mode = "x"
		}
		fmt.Fprintf(h, "%s %x %s\n", mode, e.sum, e.rel)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ReadRecord reads an installed package's install record (fs.ErrNotExist
// when it has none).
func ReadRecord(dir string) (Record, error) {
	raw, err := os.ReadFile(filepath.Join(dir, RecordFile))
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil || r.Tree == "" {
		return Record{}, fmt.Errorf("%s is unreadable: %v", RecordFile, err)
	}
	return r, nil
}

func writeRecord(dir string, r Record) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, RecordFile), append(raw, '\n'), 0o644)
}

// pinned reports whether the policy pins one of the record's digests for
// the package.
func (p *Policy) pinned(name string, r Record) bool {
	for _, d := range p.Pinned[name] {
		if d == r.Tree || r.Archive != "" && d == r.Archive {
			return true
		}
	}
	return false
}

// publishers are the publishers trusted for the package.
func (p *Policy) publishers(name string) []Publisher {
	var out []Publisher
	for _, pub := range p.Publishers {
		if pub.covers(name) {
			out = append(out, pub)
		}
	}
	return out
}

// Admits checks a package's install record against the policy: a pinned
// digest, or a recorded signer a publisher trusted for the package
// matches. A nil policy admits anything.
func (p *Policy) Admits(name string, r Record) error {
	if p == nil || p.pinned(name, r) {
		return nil
	}
	if r.Signer != nil {
		for _, pub := range p.publishers(name) {
			if pub.matches(*r.Signer) {
				return nil
			}
		}
		return fmt.Errorf("%w: %s was signed by %s, which settings.extension_trust doesn't trust for it", ErrProvenance, name, r.Signer)
	}
	return fmt.Errorf("%w: %s was installed without a verified signature, and settings.extension_trust pins no digest of it (sha256:%s)", ErrProvenance, name, r.Tree)
}

// admit decides an install under the policy. A pin admits the package
// outright; otherwise the signature bundle (nil when none was found) must
// verify over the archive as a publisher trusted for the package. It
// returns the record to write.
func (o InstallOptions) admit(ctx context.Context, name string, r Record) (Record, error) {
	p := o.Policy
	if p == nil {
		return r, nil
	}
	if p.pinned(name, r) {
		r.Pinned = true
		return r, nil
	}
	pubs := p.publishers(name)
	switch {
	case len(pubs) == 0:
		return r, fmt.Errorf("%w: settings.extension_trust trusts no publisher for %s and pins no digest of it (sha256:%s, archive sha256:%s)", ErrProvenance, name, r.Tree, orNone(r.Archive))
	case o.archive == "":
		return r, fmt.Errorf("%w: %s is a directory, which carries no signature, and settings.extension_trust pins no digest of it (sha256:%s); install its signed release archive or pin the digest", ErrProvenance, name, r.Tree)
	case o.bundle == nil:
		return r, fmt.Errorf("%w: no signature bundle was found for %s (looked for %s); pass --signature, or pin its digest (archive sha256:%s)", ErrProvenance, r.Source, o.bundleAt, r.Archive)
	case o.Verify == nil:
		return r, fmt.Errorf("%w: this build of open-conduit cannot verify signatures", ErrProvenance)
	}
	artifact, err := os.ReadFile(o.archive)
	if err != nil {
		return r, err
	}
	var errs []string
	for _, pub := range pubs {
		got, err := o.Verify(ctx, o.bundle, artifact, pub.Signer)
		if err == nil {
			r.Signer = &got
			return r, nil
		}
		errs = append(errs, pub.Signer.String()+": "+err.Error())
	}
	return r, fmt.Errorf("%w: %s's signature doesn't verify as a publisher settings.extension_trust trusts for it: %s", ErrProvenance, name, strings.Join(errs, "; "))
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// Provenance is an installed package's state under a policy, for start,
// extension list and doctor.
type Provenance struct {
	// Record is the install record, nil when there is none.
	Record *Record
	// Tree is the package tree's digest now.
	Tree string
	// Changed is set when the tree no longer matches the record.
	Changed bool
	// Refused is why the policy refuses the package (nil: admitted).
	Refused error
	// Warning is a problem the absence of a policy leaves admitted (a
	// changed tree).
	Warning string
}

// Publisher describes who the record says published the package.
func (p Provenance) Publisher() string {
	switch {
	case p.Record == nil:
		return "no install record"
	case p.Record.Signer != nil:
		return "signed by " + p.Record.Signer.String()
	case p.Record.Pinned:
		return "pinned digest"
	}
	return "unverified"
}

// CheckProvenance compares an installed package directory with its record
// and the policy. Without a policy, nothing is refused: a missing record is
// as before, and a changed tree is only a warning.
func CheckProvenance(dir, name string, p *Policy) Provenance {
	var out Provenance
	tree, err := TreeDigest(dir)
	out.Tree = tree
	if err != nil {
		if p != nil {
			out.Refused = fmt.Errorf("%w: %s: %w", ErrProvenance, name, err)
		}
		return out
	}
	rec, err := ReadRecord(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if p != nil {
			out.Refused = fmt.Errorf("%w: %s has no install record (it was installed before settings.extension_trust or copied in by hand); reinstall it with open-conduit extension update", ErrProvenance, name)
		}
		return out
	case err != nil:
		out.Refused = fmt.Errorf("%w: %s: %w", ErrProvenance, name, err)
		if p == nil {
			out.Refused, out.Warning = nil, err.Error()
		}
		return out
	}
	out.Record = &rec
	if rec.Tree != tree {
		out.Changed = true
		msg := fmt.Sprintf("%s changed on disk since it was installed (sha256:%s, installed as sha256:%s)", name, tree, rec.Tree)
		if p == nil {
			out.Warning = msg
		} else {
			out.Refused = fmt.Errorf("%w: %s", ErrProvenance, msg)
		}
		return out
	}
	out.Refused = p.Admits(name, rec)
	return out
}
