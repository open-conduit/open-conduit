package extproc

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/open-conduit/open-conduit/protocol"
)

// Package layout (07-03, provisional). A package is one directory:
//
//	<name>/
//	  open-conduit-extension.json   the manifest (required); <name> is its name
//	  <command>                     the manifest's command[0], relative, executable
//	  <playbooks>/                  the manifest's playbooks directory, if declared
//	  certification.json            the conformance runner's fixture (optional)
//	  ...                           anything else the command needs
//
// Installed packages live in <config dir>/extensions/<name>/ (Dir). The
// daemon reads them only at start, so install, update, rollback and removal
// take effect at the next `open-conduit serve`. Entries whose names begin
// with "." in the install root are the installer's own (staging, the
// previous release kept for rollback) and are never discovered.

// CertificationFile is the optional fixture file of a package.
const CertificationFile = "certification.json"

const (
	previousDir = ".previous"
	stagingDir  = ".staging"
)

// Package is a package directory read and checked offline.
type Package struct {
	Dir      string
	Manifest protocol.Manifest
	Raw      []byte
	// Digest is the SHA-256 of the manifest file and command binary, for
	// reports and update diagnostics.
	Digest string
}

// ErrPackage wraps every package layout failure.
var ErrPackage = errors.New("extproc: invalid extension package")

// ReadPackage reads a package directory and checks its layout: a valid
// manifest, a relative command inside the directory that exists and is
// executable, and the declared playbooks directory.
func ReadPackage(dir string) (Package, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return Package{}, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, protocol.ManifestFile))
	if err != nil {
		return Package{}, fmt.Errorf("%w: %s: %w", ErrPackage, protocol.ManifestFile, err)
	}
	m, err := protocol.ParseManifest(raw)
	if err != nil {
		return Package{}, err
	}
	cmd := m.Command[0]
	if filepath.IsAbs(cmd) {
		return Package{}, fmt.Errorf("%w: command %q must be a file inside the package", ErrPackage, cmd)
	}
	bin := filepath.Join(dir, cmd)
	info, err := os.Stat(bin)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return Package{}, fmt.Errorf("%w: command %q is missing or not executable", ErrPackage, cmd)
	}
	if m.Provides.Playbooks != "" {
		if info, err := os.Stat(filepath.Join(dir, m.Provides.Playbooks)); err != nil || !info.IsDir() {
			return Package{}, fmt.Errorf("%w: playbooks directory %q is missing", ErrPackage, m.Provides.Playbooks)
		}
	}
	h := sha256.New()
	h.Write(raw)
	if f, err := os.Open(bin); err == nil {
		_, _ = io.Copy(h, f)
		f.Close()
	}
	return Package{Dir: dir, Manifest: m, Raw: raw, Digest: hex.EncodeToString(h.Sum(nil))}, nil
}

// Found returns the package as a discovered extension.
func (p Package) Found() Found { return Found{Dir: p.Dir, Manifest: p.Manifest, Raw: p.Raw} }

// InstallOptions control Install.
type InstallOptions struct {
	// Update allows replacing an installed package of the same name; the
	// replaced release is kept for Rollback.
	Update bool
	// Taken are the names registered in-process (first-party extensions,
	// executors, builtins); a package claiming one is refused.
	Taken Taken
}

// InstallResult describes a completed install or update.
type InstallResult struct {
	Name, Version, Dir string
	// Previous is the replaced release's version ("" for a new install).
	Previous string
	// Permissions are those the installed release's manifest declares.
	Permissions []string
}

// ErrInstalled refuses installing over an installed package without Update.
var ErrInstalled = errors.New("extproc: an extension of that name is installed (use update)")

// ErrNotInstalled refuses removing, updating or rolling back a package
// that is not installed.
var ErrNotInstalled = errors.New("extproc: no extension of that name is installed")

// ErrIncompatiblePackage refuses a package whose protocol versions the
// host does not speak, or whose KV layout is older than the installed one.
var ErrIncompatiblePackage = errors.New("extproc: the package is incompatible with this host or the installed release")

// Install copies a package into root/<name>. Rules:
//   - the package layout and manifest are valid (ReadPackage);
//   - its command, when it is a compiled executable, is built for this
//     host's OS and architecture;
//   - it speaks a protocol version this host speaks;
//   - its names conflict neither with in-process registrations nor with
//     another installed package (atomic, as at discovery);
//   - a package of the same name is replaced only with Update, never by
//     one declaring an older kv_version, and the replaced release is kept
//     as root/.previous/<name> for Rollback.
//
// The copy is staged beside the install root and renamed into place, so a
// failed install leaves the installed release untouched.
func Install(root, src string, o InstallOptions) (InstallResult, error) {
	pkg, err := ReadPackage(src)
	if err != nil {
		return InstallResult{}, err
	}
	if err := checkPlatform(filepath.Join(pkg.Dir, pkg.Manifest.Command[0])); err != nil {
		return InstallResult{}, err
	}
	m := pkg.Manifest
	if _, err := protocol.Negotiate(m.Protocol, protocol.Versions); err != nil {
		return InstallResult{}, fmt.Errorf("%w: it speaks protocol %v, this host speaks %v", ErrIncompatiblePackage, m.Protocol, protocol.Versions)
	}
	installed, errs := Discover(root)
	if len(errs) > 0 {
		return InstallResult{}, errors.Join(errs...)
	}
	set := []Found{pkg.Found()}
	var current *Found
	for i := range installed {
		if installed[i].Manifest.Name == m.Name {
			current = &installed[i]
			continue
		}
		set = append(set, installed[i])
	}
	if current != nil && !o.Update {
		return InstallResult{}, fmt.Errorf("%w: %s %s", ErrInstalled, m.Name, current.Manifest.Version)
	}
	if current == nil && o.Update {
		return InstallResult{}, fmt.Errorf("%w: %s", ErrNotInstalled, m.Name)
	}
	if current != nil && m.KVVersion < current.Manifest.KVVersion {
		return InstallResult{}, fmt.Errorf("%w: %w: kv_version %d is older than the installed %d", ErrIncompatiblePackage, protocol.ErrKVDowngrade, m.KVVersion, current.Manifest.KVVersion)
	}
	if err := CheckConflicts(set, o.Taken); err != nil {
		return InstallResult{}, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return InstallResult{}, err
	}
	stage := filepath.Join(root, stagingDir, m.Name+"-"+newID()[:8])
	if err := copyTree(pkg.Dir, stage); err != nil {
		os.RemoveAll(stage)
		return InstallResult{}, err
	}
	if _, err := ReadPackage(stage); err != nil {
		os.RemoveAll(stage)
		return InstallResult{}, err
	}
	dest := filepath.Join(root, m.Name)
	res := InstallResult{Name: m.Name, Version: m.Version, Dir: dest}
	if current != nil {
		res.Previous = current.Manifest.Version
		prev := filepath.Join(root, previousDir, m.Name)
		if err := os.MkdirAll(filepath.Dir(prev), 0o755); err != nil {
			os.RemoveAll(stage)
			return InstallResult{}, err
		}
		if err := os.RemoveAll(prev); err != nil {
			os.RemoveAll(stage)
			return InstallResult{}, err
		}
		if err := os.Rename(dest, prev); err != nil {
			os.RemoveAll(stage)
			return InstallResult{}, err
		}
	}
	if err := os.Rename(stage, dest); err != nil {
		if current != nil {
			_ = os.Rename(filepath.Join(root, previousDir, m.Name), dest)
		}
		os.RemoveAll(stage)
		return InstallResult{}, err
	}
	_ = os.Remove(filepath.Join(root, stagingDir))
	return res, nil
}

// Rollback swaps the installed release with the one Install kept. The host
// still refuses the restored release at start if the newer one migrated the
// extension's KV (kv_downgrade); the remedy is then a 06-02 backup restore.
func Rollback(root, name string) (InstallResult, error) {
	dest := filepath.Join(root, name)
	prev := filepath.Join(root, previousDir, name)
	old, err := ReadPackage(prev)
	if err != nil {
		return InstallResult{}, fmt.Errorf("%w: no previous release of %s is kept", ErrNotInstalled, name)
	}
	cur, err := ReadPackage(dest)
	if err != nil {
		return InstallResult{}, fmt.Errorf("%w: %s", ErrNotInstalled, name)
	}
	swap := filepath.Join(root, stagingDir, name+"-rollback-"+newID()[:8])
	if err := os.MkdirAll(filepath.Dir(swap), 0o755); err != nil {
		return InstallResult{}, err
	}
	if err := os.Rename(dest, swap); err != nil {
		return InstallResult{}, err
	}
	if err := os.Rename(prev, dest); err != nil {
		_ = os.Rename(swap, dest)
		return InstallResult{}, err
	}
	if err := os.Rename(swap, prev); err != nil {
		return InstallResult{}, err
	}
	_ = os.Remove(filepath.Join(root, stagingDir))
	return InstallResult{Name: name, Version: old.Manifest.Version, Dir: dest, Previous: cur.Manifest.Version, Permissions: old.Manifest.Permissions}, nil
}

// Remove deletes an installed package and the release kept for rollback.
// It never touches the extension's durable state: its KV namespaces and
// recorded kv_version stay in the coordination store, and its state and log
// directories stay under the state directory, so reinstalling the same or a
// newer release resumes from them. Purge (Paths) names those directories for
// an operator who wants them gone. The caller refuses removal while the
// configuration still uses the package's types.
func Remove(root, name string) (InstallResult, error) {
	dest := filepath.Join(root, name)
	pkg, err := ReadPackage(dest)
	if err != nil {
		return InstallResult{}, fmt.Errorf("%w: %s", ErrNotInstalled, name)
	}
	if err := os.RemoveAll(dest); err != nil {
		return InstallResult{}, err
	}
	_ = os.RemoveAll(filepath.Join(root, previousDir, name))
	return InstallResult{Name: name, Version: pkg.Manifest.Version, Dir: dest, Permissions: pkg.Manifest.Permissions}, nil
}

// Paths are an extension's host-side directories under the state directory.
type Paths struct{ State, Log string }

// StatePaths returns where the host keeps an extension's private state
// directory and captured stderr.
func StatePaths(stateDir, name string) Paths {
	return Paths{State: filepath.Join(stateDir, "extensions", name), Log: filepath.Join(stateDir, "logs", "extensions", name+".log")}
}

// Installed lists the installed packages (and the previous release kept for
// each), in name order, with discovery problems.
type Installed struct {
	Package
	Previous string
}

// List reads every installed package.
func List(root string) ([]Installed, []error) {
	found, errs := Discover(root)
	out := make([]Installed, 0, len(found))
	for _, f := range found {
		pkg, err := ReadPackage(f.Dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		in := Installed{Package: pkg}
		if prev, err := ReadPackage(filepath.Join(root, previousDir, f.Manifest.Name)); err == nil {
			in.Previous = prev.Manifest.Version
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.Name < out[j].Manifest.Name })
	return out, errs
}

// copyTree copies a package directory, keeping file modes and symlinks to
// regular files resolved (an installed package is self-contained).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", ErrPackage, rel)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		return os.Chtimes(target, time.Now(), info.ModTime())
	})
}
