package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// travelingDirs are what a bundle leaves out of the state directory and a
// move has to decide about, with what is lost if they stay behind.
var travelingDirs = []management.PortableDirectory{
	{Name: "homes", Why: "containerised agents' sessions: without it they resume from a handoff note"},
	{Name: "executors", Why: "executors' private run state: attempts recover or retry without it"},
	{Name: "extensions", Why: "packages' private caches, which they rebuild"},
	{Name: "resources", Why: "workspaces: unpushed work in them is lost, and worktrees stay registered in the old checkout"},
}

// pathKeys are config fields that name a place on this machine even when
// nothing is there now. Any other absolute string counts only if it exists
// here, which keeps a webhook's URL path out of the report.
var pathKeys = map[string]bool{"checkout": true, "dir": true, "read": true, "write": true, "includes": true, "plugin_dir": true, "binary": true, "command": true, "base_dir": true}

// portability is GET /portability.
func (d *daemon) portability(ctx context.Context) (management.Portability, error) {
	out := management.Portability{Secrets: []management.PortableSecret{}, Paths: []management.PortablePath{}, Workspaces: []management.PortableWorkspace{}, Packages: []management.PortablePackage{}, Directories: []management.PortableDirectory{}}
	if err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) (err error) {
		out.Drain, err = runtime.DrainStatusInTx(tx)
		return err
	}); err != nil {
		return out, err
	}
	snapshot, _ := d.configs.Current()
	// A stored: reference travels when the secrets file, which moves with the
	// daemon, is what holds its value. A name it can't confirm is reported.
	inFile, _ := (&config.SecretStore{File: config.SecretsPath(d.o.ConfigPath)}).FileNames()
	out.Secrets, out.Paths = machineConfig(snapshot.Normalized, inFile)
	providers := d.resources.providers.all()
	for _, name := range providerNames(providers) {
		r, ok := providers[name].(extension.ResourceUnsavedReporter)
		if !ok {
			out.Unchecked = append(out.Unchecked, "resource provider "+name+" can't report unsaved work")
			continue
		}
		found, err := r.Unsaved(ctx)
		if err != nil {
			out.Unchecked = append(out.Unchecked, fmt.Sprintf("resource provider %s: %v", name, err))
			continue
		}
		for _, w := range found {
			out.Workspaces = append(out.Workspaces, management.PortableWorkspace{Provider: name, Ref: w.Ref, Mode: w.Mode, Path: w.Path, Site: w.Site, Kept: w.Kept})
		}
	}
	// From the manifests alone: a package that wouldn't start still has to
	// be installed on the new machine.
	installed, errs := extproc.Discover(extensionsRoot(d.o.ConfigPath))
	for _, p := range installed {
		out.Packages = append(out.Packages, management.PortablePackage{Name: p.Manifest.Name, Version: p.Manifest.Version})
	}
	if err := errors.Join(errs...); err != nil {
		out.Unchecked = append(out.Unchecked, "installed packages: "+err.Error())
	}
	// Directories say what to carry; any real install has some, so they
	// don't count against being portable.
	out.Directories = presentDirs(d.o.StateDir)
	out.Portable = out.Drain.Safe && len(out.Secrets)+len(out.Paths)+len(out.Workspaces)+len(out.Packages)+len(out.Unchecked) == 0
	return out, nil
}

// presentDirs are the traveling directories with something in them.
func presentDirs(stateDir string) []management.PortableDirectory {
	out := []management.PortableDirectory{}
	for _, dir := range travelingDirs {
		if entries, err := os.ReadDir(filepath.Join(stateDir, dir.Name)); err == nil && len(entries) > 0 {
			out = append(out, dir)
		}
	}
	return out
}

// machineConfig walks a config document for what is tied to this machine:
// secret references that aren't file: or a stored: one whose name is in
// inFile (by location and scheme, never their text) and absolute paths.
func machineConfig(doc []byte, inFile []string) ([]management.PortableSecret, []management.PortablePath) {
	secrets, paths := []management.PortableSecret{}, []management.PortablePath{}
	var root any
	if json.Unmarshal(doc, &root) != nil {
		return secrets, paths
	}
	var walk func(at, key string, v any)
	walk = func(at, key string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, c := range v {
				if refs, ok := c.(map[string]any); ok && k == "secret_refs" {
					for name, ref := range refs {
						text, _ := ref.(string)
						scheme, rest, found := strings.Cut(text, ":")
						travels := scheme == config.StoreFile || scheme == config.StoredScheme && slices.Contains(inFile, rest)
						if !found || !travels {
							secrets = append(secrets, management.PortableSecret{Path: at + "/secret_refs/" + name, Scheme: scheme})
						}
					}
					continue
				}
				walk(at+"/"+k, k, c)
			}
		case []any:
			for i, c := range v {
				walk(fmt.Sprintf("%s/%d", at, i), key, c)
			}
		case string:
			if !filepath.IsAbs(v) && !strings.HasPrefix(v, "~/") {
				return
			}
			if !pathKeys[key] {
				if _, err := os.Lstat(v); err != nil {
					return
				}
			}
			paths = append(paths, management.PortablePath{Path: at, Value: v})
		}
	}
	walk("", "", root)
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Path < secrets[j].Path })
	sort.Slice(paths, func(i, j int) bool { return paths[i].Path < paths[j].Path })
	return secrets, paths
}
