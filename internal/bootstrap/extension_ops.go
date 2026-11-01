package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/certify"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/protocol"
)

// ExtensionOps implements `open-conduit extension …` (07-03) offline: the
// packages live in <config dir>/extensions and a running daemon picks up a
// change at its next start.
func ExtensionOps(version, commit string) *cli.ExtensionOps {
	return &cli.ExtensionOps{
		List: func(configPath string) (cli.ExtensionList, error) {
			root := extensionsRoot(configPath)
			out := cli.ExtensionList{Root: root, Packages: []cli.ExtensionPackage{}}
			installed, errs := extproc.List(root)
			for _, in := range installed {
				p := describe(in.Package)
				p.Previous = in.Previous
				out.Packages = append(out.Packages, p)
			}
			for _, err := range errs {
				out.Problems = append(out.Problems, err.Error())
			}
			// The bundled packages no installed one replaces.
			all, _ := discoverPackages(configPath, "")
			for _, f := range all {
				if !f.Bundled {
					continue
				}
				if pkg, err := extproc.ReadPackage(f.Dir); err == nil {
					p := describe(pkg)
					p.Bundled = true
					out.Packages = append(out.Packages, p)
				}
			}
			if len(all) > 0 {
				if err := extproc.CheckConflicts(all, firstPartyTaken()); err != nil {
					out.Problems = append(out.Problems, extproc.Diagnose("", err, extproc.Paths{}).Summary+": "+err.Error())
				}
			}
			return out, nil
		},
		Inspect: func(dir string) (cli.ExtensionPackage, error) {
			pkg, err := extproc.ReadPackage(dir)
			if err != nil {
				return cli.ExtensionPackage{}, diagnosed(filepath.Base(dir), err)
			}
			return describe(pkg), nil
		},
		Check: func(dir string, allow []string) cli.ExtensionCheck {
			r := extproc.Check(dir, extproc.Known{Taken: firstPartyTaken(), Probes: hostProbeNames(), Allow: allow})
			out := cli.ExtensionCheck{Dir: r.Dir, Name: r.Name, Version: r.Version, Protocol: r.Protocol, OK: r.OK, Findings: []cli.CheckFinding{}, Notes: r.Notes}
			for _, f := range r.Findings {
				out.Findings = append(out.Findings, cli.CheckFinding(f))
			}
			return out
		},
		Install: func(configPath string, r cli.InstallRequest) (cli.ExtensionChange, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			src := extproc.Source{Location: r.Source, SHA256: r.SHA256}
			res, err := extproc.InstallSource(ctx, extensionsRoot(configPath), src, extproc.InstallOptions{Update: r.Update, Taken: firstPartyTaken()})
			if err != nil {
				return cli.ExtensionChange{}, diagnosed(path.Base(r.Source), err)
			}
			action := "installed"
			if r.Update {
				action = "updated"
			}
			return cli.ExtensionChange{Action: action, Name: res.Name, Version: res.Version, Previous: res.Previous, Dir: res.Dir, Permissions: permissions(res.Permissions),
				Notes: []string{"Takes effect at the next open-conduit serve.", "Certify it with: open-conduit extension certify " + res.Dir}}, nil
		},
		Rollback: func(configPath, name string) (cli.ExtensionChange, error) {
			res, err := extproc.Rollback(extensionsRoot(configPath), name)
			if err != nil {
				return cli.ExtensionChange{}, diagnosed(name, err)
			}
			return cli.ExtensionChange{Action: "rolled back", Name: res.Name, Version: res.Version, Previous: res.Previous, Dir: res.Dir, Permissions: permissions(res.Permissions),
				Notes: []string{"Takes effect at the next open-conduit serve.", "If the newer release migrated its KV layout, the daemon refuses this one (kv_downgrade): restore a backup taken before the update instead."}}, nil
		},
		Remove: func(configPath, stateDir, name string, force bool) (cli.ExtensionChange, error) {
			if data, err := os.ReadFile(configPath); err == nil && !force {
				with, err := validateWith(configPath, "", data)
				if err != nil {
					return cli.ExtensionChange{}, err
				}
				without, err := validateWith(configPath, name, data)
				if err != nil {
					return cli.ExtensionChange{}, err
				}
				if len(with) == 0 && len(without) > 0 {
					return cli.ExtensionChange{}, fmt.Errorf("the configuration still uses %s (%s at %s); remove those entries first, or pass --force", name, without[0].Code, without[0].Path)
				}
			}
			res, err := extproc.Remove(extensionsRoot(configPath), name)
			if err != nil {
				return cli.ExtensionChange{}, diagnosed(name, err)
			}
			paths := extproc.StatePaths(stateDir, name)
			return cli.ExtensionChange{Action: "removed", Name: res.Name, Version: res.Version, Dir: res.Dir,
				Notes: []string{"Takes effect at the next open-conduit serve.", "Kept (reinstalling resumes from them): its KV namespaces and recorded layout in the coordination store, " + paths.State + " and " + paths.Log + "."}}, nil
		},
		Certify: func(ctx context.Context, r cli.CertifyRequest) (cli.CertifyResult, error) {
			o := certify.Options{Dir: r.Dir, HostVersion: version, HostCommit: commit}
			if r.Progress != nil {
				o.Progress = r.Progress
			}
			rep, err := certify.Run(ctx, o)
			if err != nil {
				return cli.CertifyResult{}, err
			}
			return cli.CertifyResult{Certified: rep.Certified, Text: rep.Text(), Report: rep}, nil
		},
		Setups: func(configPath, stateDir string) []extension.Setup {
			return extensionSetups(configPath, stateDir, version)
		},
	}
}

func diagnosed(name string, err error) error {
	d := extproc.Diagnose(name, err, extproc.Paths{})
	return fmt.Errorf("%s (%s): %w. %s. See %s", d.Summary, d.Code, err, d.Hint, d.Docs)
}

func describe(p extproc.Package) cli.ExtensionPackage {
	m := p.Manifest
	var provides []string
	add := func(kind string, n int) {
		if n > 0 {
			provides = append(provides, fmt.Sprintf("%d %s", n, kind))
		}
	}
	add("connection type(s)", len(m.Provides.Connections))
	add("extension type(s)", len(m.Provides.Extensions))
	add("route handler(s)", len(m.Provides.Routes))
	add("operation(s)", len(m.Provides.Operations))
	add("executor(s)", len(m.Provides.Executors))
	add("resource provider(s)", len(m.Provides.Resources))
	add("task(s)", len(m.Provides.Tasks))
	if m.Provides.Setup != nil {
		provides = append(provides, "setup "+m.Provides.Setup.Type)
	}
	return cli.ExtensionPackage{Name: m.Name, Version: m.Version, Summary: m.Summary, Protocol: m.Protocol, Dir: p.Dir, Digest: p.Digest, Provides: provides, Permissions: permissions(m.Permissions)}
}

// permissions describes a manifest's permissions for the operator.
func permissions(names []string) []cli.ExtensionPermission {
	var out []cli.ExtensionPermission
	for _, n := range names {
		out = append(out, cli.ExtensionPermission{Name: n, Summary: protocol.PermissionSummary(n)})
	}
	return out
}

// firstPartyTaken lists the in-process registrations' names, offline.
func firstPartyTaken() extproc.Taken {
	reg := registry(offlineExecutors())
	return takenNames(reg)
}

// extensionSetups are the installed packages' setup flows. Each starts its
// package for the duration of the flow, without a coordination store (a
// setup needs only the operator, the config document and the secret store).
func extensionSetups(configPath, stateDir, version string) []extension.Setup {
	found, _ := discoverPackages(configPath, "")
	if extproc.CheckConflicts(found, firstPartyTaken()) != nil {
		return nil
	}
	var out []extension.Setup
	for _, f := range found {
		if d := f.Manifest.Provides.Setup; d != nil {
			out = append(out, packageSetup{f: f, stateDir: stateDir, version: version, info: extension.SetupInfo{Type: d.Type, Summary: d.Summary + " (package " + f.Manifest.Name + " " + f.Manifest.Version + ")", Usage: d.Usage, FirstRun: d.FirstRun}})
		}
	}
	return out
}

type packageSetup struct {
	f        extproc.Found
	stateDir string
	version  string
	info     extension.SetupInfo
}

func (s packageSetup) SetupInfo() extension.SetupInfo { return s.info }

func (s packageSetup) Setup(ctx context.Context, host extension.SetupHost, args []string) (extension.SetupResult, error) {
	paths := extproc.StatePaths(s.stateDir, s.f.Manifest.Name)
	r, err := extproc.Load(ctx, s.f, extproc.Deps{Now: time.Now}, extproc.Options{StateDir: paths.State, LogDir: filepath.Dir(paths.Log), Host: protocol.HostInfo{Name: "open-conduit", Version: s.version}})
	if err != nil {
		return extension.SetupResult{}, diagnosed(s.f.Manifest.Name, err)
	}
	defer r.Process().Close()
	setup, ok := r.Setup()
	if !ok {
		return extension.SetupResult{}, errors.New("the package declares no setup")
	}
	return setup.Setup(ctx, host, args)
}
