// Package extproc is the host runtime for third-party extension processes
// (07-02): discovery of manifests, atomic conflict checks against
// first-party registrations, supervised processes speaking protocol version
// 1, the scoped extension-to-host service, and adapters that present a
// process through the same public contracts in-process extensions implement
// (extension.Ingress, Executor, RouteHandler, Registration and the runtime's
// callback types). Nothing in core distinguishes an external extension once
// it is adapted.
//
// Trust model (site/src/content/docs/architecture/extension-protocol.md): an extension
// process runs with the operator's user privileges. Process separation is
// fault isolation, not a sandbox. What the host enforces is the protocol
// surface: every extension-to-host call is scoped by the identity bound at
// start, and core records are reached only through checks the in-process
// callers also pass.
package extproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// Dir is the install location under the configuration directory (beside
// config.json): <config dir>/extensions/<name>/open-conduit-extension.json.
// Provisional (07-02); install, update, rollback and removal are Install,
// Rollback and Remove (07-03).
const Dir = "extensions"

// Found is one discovered extension: its directory, parsed manifest and the
// exact file bytes.
type Found struct {
	Dir      string
	Manifest protocol.Manifest
	Raw      []byte
	// Bundled marks a package that ships with the binary rather than one
	// installed beside the config.
	Bundled bool
}

// Discover reads every <root>/<dir>/open-conduit-extension.json, in
// directory order, validating each manifest offline (no process starts).
// A missing root is no extensions. Invalid manifests are reported and
// skipped; the directory must be named after the manifest's name.
func Discover(root string) ([]Found, []error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	var found []Found
	var errs []error
	for _, e := range entries {
		// Dot entries are the installer's (staging, kept releases).
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, protocol.ManifestFile))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		m, err := protocol.ParseManifest(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		if m.Name != e.Name() {
			errs = append(errs, fmt.Errorf("%s: %w: directory must be named %q", e.Name(), protocol.ErrManifest, m.Name))
			continue
		}
		found = append(found, Found{Dir: dir, Manifest: m, Raw: raw})
	}
	return found, errs
}

// Taken lists names already registered in-process (first-party extensions,
// executors, builtins, core). Keys are kinds: "name", "type", "handler",
// "executor", "resource", "operation", "family", "work_kind", "recurrence",
// "schema".
type Taken map[string]map[string]bool

// Add records names of one kind.
func (t Taken) Add(kind string, names ...string) {
	if t[kind] == nil {
		t[kind] = map[string]bool{}
	}
	for _, n := range names {
		t[kind][n] = true
	}
}

// ErrConflict wraps a registration conflict.
var ErrConflict = errors.New("extproc: registration conflict")

func claims(m protocol.Manifest) map[string][]string {
	p := m.Provides
	c := map[string][]string{"name": {m.Name}}
	for _, x := range p.Connections {
		c["type"] = append(c["type"], x.Type)
	}
	for _, x := range p.Extensions {
		c["type"] = append(c["type"], x.Type)
	}
	if p.Setup != nil {
		c["type"] = append(c["type"], p.Setup.Type)
	}
	for _, x := range p.Routes {
		c["handler"] = append(c["handler"], x.Handler)
	}
	for _, x := range p.Executors {
		c["executor"] = append(c["executor"], x.Name)
	}
	for _, x := range p.Resources {
		c["resource"] = append(c["resource"], x.Type)
	}
	for _, x := range p.Operations {
		c["operation"] = append(c["operation"], x.Name)
	}
	for _, x := range p.Families {
		c["family"] = append(c["family"], x.Type)
	}
	for _, x := range p.WorkKinds {
		c["work_kind"] = append(c["work_kind"], x.Kind)
	}
	c["recurrence"] = append(c["recurrence"], p.Recurrences...)
	for _, x := range p.Schemas {
		c["schema"] = append(c["schema"], x.Name)
	}
	return c
}

// CheckConflicts refuses, atomically, any extension set in which a name is
// already taken in-process or claimed twice: either every extension is
// admitted or none is. Setup types share the config type space, and a
// manifest's own repeated use of a name across kinds (an extension type
// named after the extension) is not a conflict.
func CheckConflicts(found []Found, taken Taken) error {
	var errs []string
	owner := map[string]string{}
	for _, f := range found {
		for kind, names := range claims(f.Manifest) {
			mine := map[string]bool{}
			for _, n := range names {
				if mine[n] {
					continue
				}
				mine[n] = true
				key := kind + "\x00" + n
				if taken[kind][n] {
					errs = append(errs, fmt.Sprintf("%s: %s %q is already registered in-process", f.Manifest.Name, kind, n))
				}
				if other, ok := owner[key]; ok {
					errs = append(errs, fmt.Sprintf("%s: %s %q is also claimed by %s", f.Manifest.Name, kind, n, other))
				}
				owner[key] = f.Manifest.Name
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("%w: %s", ErrConflict, strings.Join(errs, "; "))
	}
	return nil
}

// StoreKV records extension KV layout versions in the reserved core
// namespace core.extensions (key "<name>/kv_version"), which no extension
// can address.
type StoreKV struct {
	Store storage.Store
	Now   func() time.Time
}

const kvNamespace = "core.extensions"

func (s StoreKV) StoredKV(ctx context.Context, name string) (int, error) {
	var v int
	err := s.Store.Transact(ctx, storage.Guard{Now: s.Now()}, func(tx storage.Tx) error {
		raw, err := tx.Get(kvNamespace, name+"/kv_version")
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		v, err = strconv.Atoi(string(raw))
		return err
	})
	return v, err
}

func (s StoreKV) RecordKV(ctx context.Context, name string, version int) error {
	return s.Store.Transact(ctx, storage.Guard{Now: s.Now()}, func(tx storage.Tx) error {
		return tx.Put(kvNamespace, name+"/kv_version", []byte(strconv.Itoa(version)))
	})
}
