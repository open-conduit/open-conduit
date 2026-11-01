package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extension"
)

// Extension packages (07-03): install, update, rollback, removal, listing,
// inspection and the conformance runner. The work is the open-conduit
// entrypoint's (ExtensionOps); this package only parses and prints.

// ExtensionPackage describes one installed or inspected package.
type ExtensionPackage struct {
	Name     string   `json:"name"`
	Version  string   `json:"version"`
	Summary  string   `json:"summary,omitempty"`
	Protocol []int    `json:"protocol"`
	Dir      string   `json:"dir"`
	Digest   string   `json:"digest,omitempty"`
	Provides []string `json:"provides,omitempty"`
	// Permissions are the host services beyond its own surfaces the
	// package may use, each with what it allows.
	Permissions []ExtensionPermission `json:"permissions,omitempty"`
	// Previous is the release kept for rollback.
	Previous string `json:"previous,omitempty"`
	// Bundled marks a package that ships with the binary; one installed
	// under the same name replaces it.
	Bundled bool `json:"bundled,omitempty"`
}

// ExtensionPermission is one permission a package declares.
type ExtensionPermission struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

// ExtensionList is `extension list`.
type ExtensionList struct {
	Root     string             `json:"root"`
	Packages []ExtensionPackage `json:"packages"`
	Problems []string           `json:"problems,omitempty"`
}

// ExtensionChange reports an install, update, rollback or removal.
type ExtensionChange struct {
	Action   string `json:"action"`
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	Previous string `json:"previous,omitempty"`
	Dir      string `json:"dir"`
	// Permissions are those the installed release declares.
	Permissions []ExtensionPermission `json:"permissions,omitempty"`
	Notes       []string              `json:"notes,omitempty"`
}

// InstallRequest installs or updates a package from Source: a package
// directory, or a release archive (.tar.gz) given as a path or an http(s)
// URL, checked against SHA256 when set.
type InstallRequest struct {
	Source string
	SHA256 string
	Update bool
}

// CertifyRequest runs the conformance runner on a package directory.
type CertifyRequest struct {
	Dir string
	// Progress receives a line per criterion (nil: quiet).
	Progress interface{ Write([]byte) (int, error) }
}

// CertifyResult is a certification report: Text for a terminal, Report for
// --json and --report files.
type CertifyResult struct {
	Certified bool
	Text      string
	Report    any
}

// ExtensionCheck is `extension check`: every problem found in a package
// directory, each with what to change.
type ExtensionCheck struct {
	Dir      string         `json:"dir"`
	Name     string         `json:"name,omitempty"`
	Version  string         `json:"version,omitempty"`
	Protocol []int          `json:"protocol,omitempty"`
	OK       bool           `json:"ok"`
	Findings []CheckFinding `json:"findings"`
	Notes    []string       `json:"notes,omitempty"`
}

// CheckFinding is one problem: the check it came from, a stable code, what
// is wrong, the change that fixes it and the docs section on it. A warning
// does not stop install or start.
type CheckFinding struct {
	Check   string `json:"check"`
	Code    string `json:"code"`
	Problem string `json:"problem"`
	Fix     string `json:"fix"`
	Docs    string `json:"docs"`
	Warning bool   `json:"warning,omitempty"`
}

// NewRequest is `extension new`: a package repository named Name in Dir,
// with Go module path Module (default example.com/<name>). Replace, when
// set, is an Open Conduit checkout go.mod builds against.
type NewRequest struct {
	Name    string
	Dir     string
	Module  string
	Replace string
}

// NewResult is the repository `extension new` wrote.
type NewResult struct {
	Name   string `json:"name"`
	Dir    string `json:"dir"`
	Module string `json:"module"`
	// Requires is the Open Conduit version go.mod requires.
	Requires string   `json:"requires"`
	Files    []string `json:"files"`
	Notes    []string `json:"notes,omitempty"`
}

// ExtensionOps are supplied by the open-conduit entrypoint.
type ExtensionOps struct {
	List    func(configPath string) (ExtensionList, error)
	Inspect func(dir string) (ExtensionPackage, error)
	// Check lints a package directory offline; allow are names its
	// playbooks may mention that are not visible offline.
	Check    func(dir string, allow []string) ExtensionCheck
	Install  func(configPath string, r InstallRequest) (ExtensionChange, error)
	Rollback func(configPath, name string) (ExtensionChange, error)
	Remove   func(configPath, stateDir, name string, force bool) (ExtensionChange, error)
	Certify  func(ctx context.Context, r CertifyRequest) (CertifyResult, error)
	// New writes a package repository (extension new).
	New func(r NewRequest) (NewResult, error)
	// Dev runs a package against a throwaway daemon until ctx ends (dev).
	Dev func(ctx context.Context, r DevRequest) error
	// Setups returns the installed packages' setup flows (init TYPE).
	Setups func(configPath, stateDir string) []extension.Setup
}

const extensionHelp = `Usage: open-conduit extension COMMAND [--config PATH]
  new NAME [--dir DIR] [--module PATH] [--replace DIR] [--json]
                                start a package repository in DIR (default ./NAME) whose make check passes first time:
                                one operation, NAME.hello, its tests, a Makefile, CI, a playbook and a README;
                                go.mod requires this release of open-conduit (--replace builds against a checkout instead)
  list [--json]                 installed packages (in <config dir>/extensions)
  inspect DIR [--json]          check a package directory offline
  check DIR [--json] [--allow NAME,…]
                                lint a package without starting it: manifest, compatibility with this binary,
                                the operations its playbooks name, certification.json; says what to change for each problem
  install SOURCE | update SOURCE [--sha256 HEX]
                                install a package, or replace an installed one (the old release is kept for rollback);
                                SOURCE is a package directory, or a release .tar.gz as a path or URL (plain http needs --sha256)
  rollback NAME                 swap back the release the last update replaced
  remove NAME [--force]         remove a package (refused while the config uses its types; its state is kept)
  certify DIR [--json] [--report FILE]   run the conformance runner on a package
Changes take effect at the next open-conduit serve.
`

// boolFlags removes the named boolean flags from args.
func boolFlags(args []string, names ...string) ([]string, map[string]bool) {
	set := map[string]bool{}
	var rest []string
	for _, a := range args {
		matched := false
		for _, n := range names {
			if a == "--"+n || a == "-"+n {
				set[n], matched = true, true
			}
		}
		if !matched {
			rest = append(rest, a)
		}
	}
	return rest, set
}

func (o *operator) extension(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprint(o.out, extensionHelp)
		return 0
	}
	ops := o.env.Extensions
	if ops == nil {
		fmt.Fprintln(o.errOut, "open-conduit: extension packages are not available in this binary")
		return 1
	}
	sub := args[0]
	rest, bools := boolFlags(args[1:], "json", "force")
	f := newFlags(sub, "config", "report", "sha256", "allow", "dir", "module", "replace")
	pos, err := f.parse(rest)
	if err != nil {
		return usageError(o.errOut, "invalid extension flags")
	}
	path := o.configPath(f.get("config"))
	one := func(what string) (string, bool) {
		if len(pos) != 1 {
			return "", false
		}
		if what == "DIR" {
			return o.abs(pos[0]), true
		}
		return pos[0], true
	}
	switch sub {
	case "new":
		name, ok := one("NAME")
		if !ok {
			return usageError(o.errOut, "expected extension new NAME [--dir DIR] [--module PATH] [--replace DIR]")
		}
		r := NewRequest{Name: name, Dir: o.abs(name), Module: f.get("module")}
		if d := f.get("dir"); d != "" {
			r.Dir = o.abs(d)
		}
		if d := f.get("replace"); d != "" {
			r.Replace = o.abs(d)
		}
		if ops.New == nil {
			fmt.Fprintln(o.errOut, "open-conduit: extension new is not available in this binary")
			return 1
		}
		res, err := ops.New(r)
		if err != nil || bools["json"] {
			return o.emit(res, err)
		}
		fmt.Fprintf(o.out, "Created the %s package in %s (module %s, open-conduit %s)\n", res.Name, res.Dir, res.Module, res.Requires)
		for _, n := range res.Notes {
			fmt.Fprintf(o.out, "  %s\n", n)
		}
		fmt.Fprintf(o.out, "Next: cd %s && make check, then see its README for what to change first.\n", res.Dir)
		return 0
	case "list":
		l, err := ops.List(path)
		if err != nil || bools["json"] {
			return o.emit(l, err)
		}
		fmt.Fprintf(o.out, "Extension packages in %s\n", l.Root)
		if len(l.Packages) == 0 {
			fmt.Fprintln(o.out, "  none installed")
		}
		w := tabwriter.NewWriter(o.out, 2, 4, 2, ' ', 0)
		for _, p := range l.Packages {
			prev := ""
			if p.Previous != "" {
				prev = " (rollback to " + p.Previous + ")"
			}
			if p.Bundled {
				prev = " (bundled with open-conduit)"
			}
			fmt.Fprintf(w, "  %s\t%s%s\tprotocol %v\t%s\n", p.Name, p.Version, prev, p.Protocol, strings.Join(p.Provides, ", "))
			if len(p.Permissions) > 0 {
				fmt.Fprintf(w, "  \t\t\tpermissions: %s\n", permissionNames(p.Permissions))
			}
		}
		_ = w.Flush()
		for _, p := range l.Problems {
			fmt.Fprintf(o.out, "  problem: %s\n", p)
		}
		return 0
	case "inspect":
		dir, ok := one("DIR")
		if !ok {
			return usageError(o.errOut, "expected extension inspect DIR")
		}
		p, err := ops.Inspect(dir)
		if err != nil || bools["json"] {
			return o.emit(p, err)
		}
		fmt.Fprintf(o.out, "%s %s (protocol %v): valid package\n  %s\n  provides: %s\n  digest sha256:%s\n", p.Name, p.Version, p.Protocol, p.Summary, strings.Join(p.Provides, ", "), p.Digest)
		o.permissions(p.Permissions)
		return 0
	case "check":
		dir, ok := one("DIR")
		if !ok {
			return usageError(o.errOut, "expected extension check DIR [--json] [--allow NAME,…]")
		}
		var allow []string
		for _, a := range strings.Split(f.get("allow"), ",") {
			if a = strings.TrimSpace(a); a != "" {
				allow = append(allow, a)
			}
		}
		c := ops.Check(dir, allow)
		if bools["json"] {
			o.emit(c, nil)
		} else {
			o.printCheck(c)
		}
		if !c.OK {
			return 1
		}
		return 0
	case "install", "update":
		if len(pos) != 1 {
			return usageError(o.errOut, "expected extension "+sub+" DIR|ARCHIVE|URL [--sha256 HEX]")
		}
		source := pos[0]
		if !strings.Contains(source, "://") {
			source = o.abs(source)
		}
		c, err := ops.Install(path, InstallRequest{Source: source, SHA256: f.get("sha256"), Update: sub == "update"})
		return o.change(c, err, bools["json"])
	case "rollback":
		name, ok := one("NAME")
		if !ok {
			return usageError(o.errOut, "expected extension rollback NAME")
		}
		c, err := ops.Rollback(path, name)
		return o.change(c, err, bools["json"])
	case "remove":
		name, ok := one("NAME")
		if !ok {
			return usageError(o.errOut, "expected extension remove NAME [--force]")
		}
		c, err := ops.Remove(path, o.stateDir(), name, bools["force"])
		return o.change(c, err, bools["json"])
	case "certify":
		dir, ok := one("DIR")
		if !ok {
			return usageError(o.errOut, "expected extension certify DIR")
		}
		cx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		req := CertifyRequest{Dir: dir}
		if !bools["json"] {
			req.Progress = o.errOut
		}
		res, err := ops.Certify(cx, req)
		if err != nil {
			return o.emit(nil, err)
		}
		if file := f.get("report"); file != "" {
			if err := writeJSONFile(o.abs(file), res.Report); err != nil {
				return o.emit(nil, fmt.Errorf("cannot write the report: %w", err))
			}
		}
		if bools["json"] {
			o.emit(res.Report, nil)
		} else {
			fmt.Fprint(o.out, res.Text)
		}
		if !res.Certified {
			return 1
		}
		return 0
	}
	return usageError(o.errOut, "unknown extension subcommand")
}

func (o *operator) printCheck(c ExtensionCheck) {
	what := c.Dir
	if c.Name != "" {
		what = c.Name + " " + c.Version + " (" + c.Dir + ")"
	}
	errs, warns := 0, 0
	for _, f := range c.Findings {
		level := "error"
		if f.Warning {
			level, warns = "warning", warns+1
		} else {
			errs++
		}
		fmt.Fprintf(o.out, "%s %s: %s (%s)\n  Fix: %s\n  Docs: %s\n", level, f.Check, f.Problem, f.Code, f.Fix, f.Docs)
	}
	for _, n := range c.Notes {
		fmt.Fprintf(o.out, "note: %s\n", n)
	}
	switch {
	case errs == 0 && warns == 0:
		fmt.Fprintf(o.out, "%s: no problems found\n", what)
	case errs == 0:
		fmt.Fprintf(o.out, "%s: %d warning(s), nothing that stops install\n", what, warns)
	default:
		fmt.Fprintf(o.out, "%s: %d error(s), %d warning(s)\n", what, errs, warns)
	}
}

func (o *operator) change(c ExtensionChange, err error, asJSON bool) int {
	if err != nil || asJSON {
		return o.emit(c, err)
	}
	switch c.Action {
	case "removed":
		fmt.Fprintf(o.out, "Removed %s %s from %s\n", c.Name, c.Version, c.Dir)
	case "rolled back":
		fmt.Fprintf(o.out, "Rolled %s back to %s (from %s)\n", c.Name, c.Version, c.Previous)
	default:
		if c.Previous != "" {
			fmt.Fprintf(o.out, "Updated %s %s → %s in %s (the previous release is kept for rollback)\n", c.Name, c.Previous, c.Version, c.Dir)
		} else {
			fmt.Fprintf(o.out, "Installed %s %s in %s\n", c.Name, c.Version, c.Dir)
		}
	}
	if c.Action != "removed" {
		o.permissions(c.Permissions)
	}
	for _, n := range c.Notes {
		fmt.Fprintf(o.out, "  %s\n", n)
	}
	return 0
}

// permissions lists what a package may use beyond its own surfaces.
func (o *operator) permissions(ps []ExtensionPermission) {
	if len(ps) == 0 {
		return
	}
	fmt.Fprintln(o.out, "  permissions:")
	for _, p := range ps {
		fmt.Fprintf(o.out, "    %s: %s\n", p.Name, p.Summary)
	}
}

func permissionNames(ps []ExtensionPermission) string {
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}

func writeJSONFile(path string, v any) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	o := &operator{out: f}
	o.emit(v, nil)
	return f.Close()
}

// validateConfig validates a config document offline, knowing the
// packages installed beside path when the entrypoint supports it.
func (o *operator) validateConfig(path string, data []byte) ([]client.Detail, error) {
	if o.env.ValidateConfigAt != nil {
		return o.env.ValidateConfigAt(path, data)
	}
	if o.env.ValidateConfig != nil {
		return o.env.ValidateConfig(data)
	}
	return nil, nil
}
