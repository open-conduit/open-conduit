package extproc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/protocol"
)

// PackagingDocs is the packaging guide; Troubleshooting is its section
// that explains every code Check and Diagnose give.
const (
	PackagingDocs   = "https://open-conduit.github.io/open-conduit/architecture/extension-packaging/"
	Troubleshooting = PackagingDocs + "#troubleshooting"
	manifestDocs    = "https://open-conduit.github.io/open-conduit/architecture/extension-protocol/#manifest"
)

// Finding is one problem `extension check` found: where, a stable code,
// what is wrong, the one change that fixes it and where the docs explain it.
type Finding struct {
	Check   string `json:"check"`
	Code    string `json:"code"`
	Problem string `json:"problem"`
	Fix     string `json:"fix"`
	Docs    string `json:"docs"`
	// Warning marks a finding that does not stop install or start.
	Warning bool `json:"warning,omitempty"`
}

// CheckReport is the result of Check.
type CheckReport struct {
	Dir      string    `json:"dir"`
	Name     string    `json:"name,omitempty"`
	Version  string    `json:"version,omitempty"`
	Protocol []int     `json:"protocol,omitempty"`
	OK       bool      `json:"ok"`
	Findings []Finding `json:"findings"`
	// Notes say what the check could not look at offline.
	Notes []string `json:"notes,omitempty"`
}

// Known is what this binary registers outside any package, for checking
// the names a package's playbooks mention: core's toolbelt operations and
// the setup probes the host runs, each by full name.
type Known struct {
	Taken  Taken
	Probes []string
	// Allow are further names a playbook may mention that the check
	// cannot see offline (the package's own setup probes).
	Allow []string
}

// Check lints a package directory offline, without starting it: the
// manifest, its compatibility with this binary, the playbooks and the
// certification fixture. It reports every problem it finds, each with what
// to change.
func Check(dir string, k Known) (r CheckReport) {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	r = CheckReport{Dir: dir, Findings: []Finding{}}
	add := func(f Finding) {
		if f.Docs == "" {
			f.Docs = Troubleshooting
		}
		r.Findings = append(r.Findings, f)
	}
	defer func() {
		r.OK = true
		for _, f := range r.Findings {
			if !f.Warning {
				r.OK = false
			}
		}
	}()
	raw, err := os.ReadFile(filepath.Join(dir, protocol.ManifestFile))
	if err != nil {
		add(Finding{Check: "manifest", Code: "invalid_package", Problem: fmt.Sprintf("%s cannot be read: %v", protocol.ManifestFile, err),
			Fix: "run the check on the package directory, the one holding " + protocol.ManifestFile})
		return r
	}
	var m protocol.Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			add(Finding{Check: "compatibility", Code: "needs_newer_host", Problem: "the manifest has field " + field + ", which this Open Conduit does not know",
				Fix: "if it is a typo, correct it; otherwise upgrade Open Conduit to the release that added it"})
		} else {
			add(Finding{Check: "manifest", Code: "invalid_package", Problem: protocol.ManifestFile + " is not valid JSON for a manifest: " + err.Error(),
				Fix: "fix the JSON so it decodes", Docs: manifestDocs})
		}
		return r
	}
	r.Name, r.Version, r.Protocol = m.Name, m.Version, m.Protocol
	if _, err := protocol.Negotiate(m.Protocol, protocol.Versions); err != nil {
		add(Finding{Check: "compatibility", Code: "incompatible_protocol", Problem: fmt.Sprintf("the manifest speaks protocol %v; this Open Conduit speaks %v", m.Protocol, protocol.Versions),
			Fix: fmt.Sprintf("list a version this host speaks in \"protocol\" (%v) and build against an SDK release for it", protocol.Versions)})
	}
	for _, rule := range manifestRules(m.Validate()) {
		if strings.HasPrefix(rule, "protocol ") {
			continue // reported above
		}
		add(Finding{Check: "manifest", Code: "invalid_package", Problem: rule, Fix: "change " + protocol.ManifestFile + " so it keeps this rule", Docs: manifestDocs})
	}
	if m.Name != "" && filepath.Base(dir) != m.Name {
		add(Finding{Check: "manifest", Code: "invalid_package", Warning: true, Problem: fmt.Sprintf("the directory is named %q but the manifest names %q", filepath.Base(dir), m.Name),
			Fix: fmt.Sprintf("build the package into a directory named %s; install renames it, but certify and discovery need the names to match", m.Name), Docs: PackagingDocs + "#package-layout"})
	}
	layout(dir, m, add)
	if err := CheckConflicts([]Found{{Dir: dir, Manifest: m, Raw: raw}}, k.Taken); err != nil {
		add(Finding{Check: "compatibility", Code: "registration_conflict", Problem: strings.TrimPrefix(err.Error(), ErrConflict.Error()+": "),
			Fix: "rename what the package declares so no name is one Open Conduit registers itself (slack, shell, github, …)"})
	}
	playbooks(dir, m, k, add)
	certification(dir, m, add, &r.Notes)
	return r
}

// manifestRules splits Validate's error into the rules it breaks.
func manifestRules(err error) []string {
	if err == nil {
		return nil
	}
	var rules []string
	var walk func(error)
	walk = func(e error) {
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, x := range j.Unwrap() {
				if x != protocol.ErrManifest {
					walk(x)
				}
			}
			return
		}
		rules = append(rules, e.Error())
	}
	walk(err)
	return rules
}

func layout(dir string, m protocol.Manifest, add func(Finding)) {
	if len(m.Command) > 0 && m.Command[0] != "" {
		cmd := m.Command[0]
		bin := filepath.Join(dir, cmd)
		info, err := os.Stat(bin)
		switch {
		case filepath.IsAbs(cmd):
			add(Finding{Check: "manifest", Code: "invalid_package", Problem: fmt.Sprintf("command %q is an absolute path", cmd),
				Fix: "make command[0] a path relative to the package directory, and ship the executable in the package"})
		case err != nil || info.IsDir():
			add(Finding{Check: "manifest", Code: "invalid_package", Problem: fmt.Sprintf("command %q is not a file in the package", cmd),
				Fix: "build the executable into the package directory at that path, or correct command[0]"})
		case info.Mode().Perm()&0o111 == 0:
			add(Finding{Check: "manifest", Code: "invalid_package", Problem: fmt.Sprintf("command %q is not executable", cmd),
				Fix: "chmod +x " + cmd})
		default:
			if err := checkPlatform(bin); err != nil {
				add(Finding{Check: "compatibility", Code: "wrong_platform", Problem: strings.TrimPrefix(err.Error(), ErrPlatform.Error()+": "),
					Fix: "build the command for this OS and architecture (GOOS and GOARCH), or check the release archive built for it"})
			}
		}
	}
	if m.Provides.Playbooks != "" {
		if info, err := os.Stat(filepath.Join(dir, m.Provides.Playbooks)); err != nil || !info.IsDir() {
			add(Finding{Check: "playbooks", Code: "invalid_package", Problem: fmt.Sprintf("the playbooks directory %q is missing", m.Provides.Playbooks),
				Fix: "create it beside the manifest with a <name>.md file per playbook, or remove \"playbooks\" from provides"})
		}
	}
}

// quotedName matches a dotted name a playbook quotes: `ns.op` or "ns.op".
var quotedName = regexp.MustCompile("[`\"]([a-z][a-z0-9_-]*(?:\\.[a-z][a-z0-9_-]*)+)[`\"]")

// fileSuffixes are name endings that make a quoted name a file, not an
// operation.
var fileSuffixes = map[string]bool{"json": true, "md": true, "yaml": true, "yml": true, "toml": true, "go": true, "js": true, "ts": true, "sh": true, "txt": true, "log": true, "lock": true, "mod": true, "sum": true, "env": true}

// playbooks checks that each operation a playbook names in a namespace
// this package or core owns is one of theirs. Names in other namespaces
// (config paths, other packages' operations) are left alone.
func playbooks(dir string, m protocol.Manifest, k Known, add func(Finding)) {
	if m.Provides.Playbooks == "" {
		return
	}
	if info, err := os.Stat(filepath.Join(dir, m.Provides.Playbooks)); err != nil || !info.IsDir() {
		return // reported by layout
	}
	books, err := Playbooks(dir, m)
	if err != nil {
		add(Finding{Check: "playbooks", Code: "invalid_package", Problem: err.Error(), Fix: "keep each playbook a readable <name>.md file under 256 KiB"})
		return
	}
	if len(books) == 0 {
		add(Finding{Check: "playbooks", Code: "no_playbooks", Warning: true, Problem: fmt.Sprintf("the playbooks directory %q holds no <name>.md playbook", m.Provides.Playbooks),
			Fix: "add a playbook named in lower case (setup.md), or remove \"playbooks\" from provides"})
		return
	}
	known, owned := map[string]bool{}, map[string]bool{m.Name: true}
	own := func(name string) {
		known[name] = true
		if ns, _, ok := strings.Cut(name, "."); ok {
			owned[ns] = true
		}
	}
	for _, o := range m.Provides.Operations {
		own(o.Name)
	}
	for op := range k.Taken["operation"] {
		own(op)
	}
	for _, p := range k.Probes {
		own(p)
	}
	for _, f := range m.Provides.Families {
		known[f.Type] = true
	}
	for _, a := range k.Allow {
		known[a] = true
	}
	names := make([]string, 0, len(books))
	for n := range books {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		seen := map[string]bool{}
		for _, match := range quotedName.FindAllStringSubmatch(books[n], -1) {
			name := match[1]
			ns, _, _ := strings.Cut(name, ".")
			if seen[name] || known[name] || !owned[ns] || fileSuffixes[name[strings.LastIndex(name, ".")+1:]] {
				continue
			}
			seen[name] = true
			add(Finding{Check: "playbooks", Code: "unknown_operation",
				Problem: fmt.Sprintf("playbook %s names %s, which is not an operation this package declares or Open Conduit registers", n, name),
				Fix:     fmt.Sprintf("declare %s under provides.operations, correct the name, or, if it is one of the package's own setup probes, pass --allow %s", name, name)})
		}
	}
}

// certification checks that certification.json parses and has a case for
// each surface the runner requires one for.
func certification(dir string, m protocol.Manifest, add func(Finding), notes *[]string) {
	const docs = PackagingDocs + "#fixtures"
	raw, err := os.ReadFile(filepath.Join(dir, CertificationFile))
	if errors.Is(err, os.ErrNotExist) {
		add(Finding{Check: "certification", Code: "no_certification", Warning: true, Problem: "the package has no " + CertificationFile,
			Fix: "add one so open-conduit extension certify can check each surface the package declares", Docs: docs})
		return
	}
	if err != nil {
		add(Finding{Check: "certification", Code: "invalid_certification", Problem: err.Error(), Fix: "make " + CertificationFile + " readable", Docs: docs})
		return
	}
	var file protocol.CertificationFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		add(Finding{Check: "certification", Code: "invalid_certification", Problem: CertificationFile + " does not parse: " + err.Error(),
			Fix: "give it a \"fixture\" object or a \"command\" that prints one, in the protocol.CertificationFile format", Docs: docs})
		return
	}
	if file.Fixture == nil {
		if len(file.Command) > 0 {
			*notes = append(*notes, CertificationFile+" names a fixture command; certify runs it, so its cases are not checked here")
			return
		}
		add(Finding{Check: "certification", Code: "invalid_certification", Problem: CertificationFile + " has neither a fixture nor a command",
			Fix: "add a \"fixture\" object, or a \"command\" that prints one", Docs: docs})
		return
	}
	fx := file.Fixture
	missing := func(problem, fix string) {
		add(Finding{Check: "certification", Code: "fixture_missing_case", Problem: problem, Fix: fix, Docs: docs})
	}
	for _, x := range m.Provides.Extensions {
		if x.Ingress && fx.Ingress == nil {
			missing(fmt.Sprintf("extension type %s takes ingress, but the fixture has no ingress case", x.Type),
				"add fixture.ingress with a source the fixture's entries register and the delivery it should accept")
			break
		}
	}
	executors := map[string]bool{}
	for _, x := range m.Provides.Executors {
		executors[x.Name] = true
	}
	switch {
	case len(executors) > 0 && fx.Executor == nil:
		missing("the package declares an executor, but the fixture has no executor case",
			"add fixture.executor naming one of the declared executors and a binding config whose attempt finishes on its own")
	case fx.Executor != nil && !executors[fx.Executor.Executor]:
		missing(fmt.Sprintf("the fixture's executor case names %q, which the package does not declare", fx.Executor.Executor),
			"name a declared executor in fixture.executor.executor")
	}
	handlers := map[string]bool{}
	for _, h := range m.Provides.Routes {
		handlers[h.Handler] = true
	}
	for _, c := range fx.Routes {
		if !handlers[c.Handler] {
			missing(fmt.Sprintf("a route case names handler %q, which the package does not declare", c.Handler), "name a declared route handler")
		}
	}
	ops := map[string]bool{}
	for _, o := range m.Provides.Operations {
		ops[o.Name] = true
	}
	for _, c := range fx.Operations {
		if !ops[c.Operation] {
			missing(fmt.Sprintf("an operation case names %q, which the package does not declare", c.Operation), "name a declared operation, or declare it")
		}
	}
}
