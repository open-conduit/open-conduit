package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// FragmentFile is where an included folder keeps its scopes.
const FragmentFile = ".open-conduit/config.json"

// Includes is what composing a config file's includes needs besides the file.
type Includes struct {
	// Base resolves relative include paths: the config file's directory.
	Base string
	// Previous is the committed generation's normalized document. Its scopes
	// from a folder that has gone stand in for it, disabled.
	Previous json.RawMessage
}

// FolderConfigurer is optionally implemented by an executor registration
// whose binding config names folders. Compose calls it once for each binding
// from an included folder, before validation: it resolves relative paths
// against folder and may default the folder a workspace-free binding (one
// without a resource) runs in. The result is what the generation stores.
type FolderConfigurer interface {
	FolderConfig(config json.RawMessage, folder string, resource bool) (json.RawMessage, error)
}

// ValidateFile composes a config file's includes (Compose) and validates the
// result. Diagnostics about an included scope point into its include, as
// includes/<i>/scopes/<j>/...
func ValidateFile(data []byte, reg Registry, inc Includes) (*Snapshot, error) {
	doc, origins, err := compose(data, reg, inc)
	if err != nil {
		return nil, err
	}
	s, err := ValidateDocument(doc, reg)
	var invalidErr *ValidationError
	if errors.As(err, &invalidErr) {
		for i, d := range invalidErr.Diagnostics {
			invalidErr.Diagnostics[i].Path = remap(d.Path, origins)
		}
		return nil, invalid(invalidErr.Diagnostics)
	}
	return s, err
}

// Compose parses a config file and replaces its includes with their
// folders' scopes. A folder scope without a parent sits under the include's
// parent; one with a parent names the include's parent or another scope of
// the same folder, so every folder scope is beneath a scope of the file and
// inherits its authorization ceiling, connections and secrets. A folder
// whose config file has gone keeps its scopes from inc.Previous, disabled,
// so deleting it never invalidates the rest of the configuration.
func Compose(data []byte, reg Registry, inc Includes) (*Document, error) {
	doc, _, err := compose(data, reg, inc)
	return doc, err
}

func compose(data []byte, reg Registry, inc Includes) (*Document, []string, error) {
	doc, err := ParseFile(data)
	if err != nil {
		return nil, nil, err
	}
	origins := make([]string, len(doc.Scopes))
	if len(doc.Includes) == 0 {
		return doc, origins, nil
	}
	var previous *Document
	if len(inc.Previous) > 0 {
		previous, _ = Parse(inc.Previous)
	}
	own, owned := map[string]bool{}, len(doc.Scopes)
	for _, sc := range doc.Scopes {
		own[sc.ID] = true
	}
	c := &checker{fragment: true}
	seen, included := map[string]bool{}, map[string]bool{}
	for i, in := range doc.Includes {
		p := ptr("includes", i)
		folder, ok := includeFolder(in.Path, inc.Base)
		if !ok {
			c.fail(p+"/path", "invalid_value", "use an absolute folder, or one relative to the config file's directory")
			continue
		}
		if seen[folder] {
			c.fail(p+"/path", "duplicate_value", "this folder is already included; include it once")
			continue
		}
		seen[folder] = true
		if !own[in.Parent] {
			c.fail(p+"/parent", "unknown_scope", "parent must be a scope defined in the config file itself")
		}
		scopes, found := c.folderScopes(folder, p, in.Parent)
		if !found && previous != nil {
			for _, sc := range previous.Scopes {
				if sc.Source == folder {
					sc.Enabled = false
					scopes = append(scopes, sc)
				}
			}
		}
		for j, sc := range scopes {
			origin := ptr(p+"/scopes", j)
			sc.Source = folder
			if found {
				for k, b := range sc.Bindings {
					if b.Disabled {
						continue
					}
					if fc, ok := reg.Executors[b.Executor].(FolderConfigurer); ok {
						config, err := fc.FolderConfig(b.Config, folder, b.Resource != nil)
						if err != nil {
							c.fail(ptr(origin+"/bindings", k)+"/config", "invalid_config", fmt.Sprintf("executor %s rejected this binding config; correct it against the executor's documented config", b.Executor))
							continue
						}
						sc.Bindings[k].Config = canonical(json.RawMessage(config))
					}
				}
			}
			doc.Scopes = append(doc.Scopes, sc)
			origins = append(origins, origin)
			included[sc.ID] = true
		}
	}
	for i, sc := range doc.Scopes[:owned] {
		if included[sc.Parent] && !own[sc.Parent] {
			c.fail(ptr("scopes", i)+"/parent", "invalid_value", "a scope of the config file cannot sit under an included folder's scope; choose a parent in the config file")
		}
	}
	if len(c.diags) > 0 {
		return nil, nil, invalid(c.diags)
	}
	doc.Includes = nil
	return doc, origins, nil
}

// folderScopes reads and checks an included folder's scopes. found is false when
// the folder or its config file does not exist.
func (c *checker) folderScopes(folder, path, parent string) (scopes []ScopeConfig, found bool) {
	data, err := readFragment(filepath.Join(folder, FragmentFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		c.fail(path+"/path", "include_unreadable", "the folder's "+FragmentFile+" must be a regular file you own, not writable by others, at most 1 MiB")
		return nil, true
	}
	root, diags := decode(data)
	for _, d := range diags {
		c.fail(strings.TrimSuffix(path+"/"+d.Path, "/"), d.Code, d.Message)
	}
	if root == nil || !c.object(root, path, []string{"version", "scopes"}) {
		return nil, true
	}
	if v := root.field("version"); c.typed(v, path+"/version", kNumber, "integer") && v.num.String() != "1" {
		c.fail(path+"/version", "unsupported_version", "version must be exactly 1")
	}
	list := root.field("scopes")
	if !c.typed(list, path+"/scopes", kArray, "array") {
		return nil, true
	}
	ids := map[string]bool{}
	for j, item := range list.items {
		sc := c.scope(item, ptr(path+"/scopes", j))
		ids[sc.ID] = true
		scopes = append(scopes, sc)
	}
	for j := range scopes {
		switch p := scopes[j].Parent; {
		case p == "":
			scopes[j].Parent = parent
		case p != parent && !ids[p]:
			c.fail(ptr(path+"/scopes", j)+"/parent", "invalid_value", "an included scope's parent is the include's parent or another scope in the same folder; omit it to use the include's parent")
		}
	}
	return scopes, true
}

// includeFolder resolves an include path: absolute, or relative to base.
func includeFolder(path, base string) (string, bool) {
	switch {
	case filepath.IsAbs(path):
		return filepath.Clean(path), true
	case strings.HasPrefix(path, "~") || !filepath.IsAbs(base):
		return "", false
	}
	return filepath.Join(base, path), true
}

// readFragment reads a folder's config file once, bounded to MaxSize. It
// lives in a working folder, often a checkout, so it may be readable by
// others but must be a regular file the daemon's user owns that nobody else
// can write.
func readFragment(path string) ([]byte, error) {
	safe := func(info os.FileInfo) error {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
			return ErrUnsafeFile
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Getuid()) {
			return ErrUnsafeFile
		}
		return nil
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err = safe(before); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, info) {
		return nil, ErrUnsafeFile
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxSize+1))
	if err == nil && len(data) > MaxSize {
		return nil, ErrUnsafeFile
	}
	return data, err
}

// remap points a diagnostic about the k-th composed scope at its include.
func remap(path string, origins []string) string {
	rest, ok := strings.CutPrefix(path, "scopes/")
	if !ok {
		return path
	}
	index, tail, _ := strings.Cut(rest, "/")
	k, err := strconv.Atoi(index)
	if err != nil || k < 0 || k >= len(origins) || origins[k] == "" {
		return path
	}
	if tail == "" {
		return origins[k]
	}
	return origins[k] + "/" + tail
}

// IncludeWarnings returns one warning per include whose folder has no config
// file: its scopes stay as last applied, disabled, until it is back.
func IncludeWarnings(data []byte, base string) []string {
	var doc struct {
		Includes []struct {
			Path string `json:"path"`
		} `json:"includes"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	var out []string
	for _, in := range doc.Includes {
		folder, ok := includeFolder(in.Path, base)
		if !ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(folder, FragmentFile)); errors.Is(err, fs.ErrNotExist) {
			out = append(out, "include "+folder+" has no "+FragmentFile+": its scopes stay as last applied, disabled, until it is back; remove the include to drop them")
		}
	}
	return out
}
