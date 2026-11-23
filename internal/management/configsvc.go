package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

const configNamespace = "core.config"

var (
	ErrGenerationConflict = errors.New("config generation conflict")
	ErrHashMismatch       = errors.New("candidate hash does not match")
	// ErrScopeInUse is a publish that would remove or reparent a scope with
	// open Work. It arrives as a *ScopeInUseError naming them.
	ErrScopeInUse = errors.New("scope in use")
	// ErrFragmentConflict is a fragment apply naming a fragment hash that
	// is no longer the include's: someone else applied to it first.
	ErrFragmentConflict = errors.New("fragment hash conflict")
	// ErrIncludeMoved is a fragment apply whose include the config file
	// has since moved under a parent the caller isn't granted.
	ErrIncludeMoved = errors.New("include moved")
)

// fragmentRetries bounds how often a fragment apply composes again because
// another apply committed while it validated.
const fragmentRetries = 5

type activation struct {
	Generation  uint64
	Hash        string
	ActivatedAt time.Time
}

// ConfigService owns validation, atomic file save and durable generation
// activation. One generation is published with its scope records in a single
// coordination transaction; a crash before that commit retains the old one.
type ConfigService struct {
	mu         sync.RWMutex
	store      storage.Store
	now        func() time.Time
	registry   config.Registry
	path       string
	current    *config.Snapshot
	generation uint64
	// fileGeneration is the last generation a config file published (at
	// start, the restored one). A config file apply based on it or later
	// was staled by fragment applies alone.
	fileGeneration uint64
	history        map[uint64]*config.Snapshot
	// defaultHash is the default configuration's hash, when OpenConfig was
	// given one that validates.
	defaultHash string
	// Adopted is set when OpenConfig replaced a stored generation that no
	// longer validates with the config file, so the host can log it.
	Adopted *Adoption
	// OnApply lets the host rebuild generation-pinned components after commit.
	OnApply func(context.Context, *config.Snapshot, uint64)
}

// Adoption records a stored generation that failed validation (typically
// after an upgrade stopped registering something it names) and was replaced
// by the config file.
type Adoption struct {
	Stored uint64
	Cause  error
}

// ErrStoredConfigRefused is a daemon on shared state refusing a stored
// generation it can't validate. It leaves that generation in place.
var ErrStoredConfigRefused = errors.New("this daemon can't serve the shared state's stored configuration")

// RefuseStoredConfig is the refusal of a stored generation on shared state,
// because of cause. refused describes packages installed on this host that it
// won't run. With any, the fix is on this host, so the refusal says why and
// doesn't suggest --adopt-config, which would change the config for every host
// because one host has a broken package. Which package a broken manifest would
// have provided can't be known, so any refusal counts. The flag still works for
// an operator who means it.
func RefuseStoredConfig(generation uint64, normalized []byte, cause error, refused []string) error {
	if config.PolicyRefused(cause) {
		return policyRefusal(generation, cause)
	}
	names := config.UnregisteredNames(normalized, cause)
	if len(refused) > 0 {
		needs := ""
		if len(names) > 0 {
			needs = "; it uses " + strings.Join(names, ", ")
		}
		return fmt.Errorf("%w: generation %d doesn't validate here%s: %w; this host refused installed packages, so repair them here: %s", ErrStoredConfigRefused, generation, needs, cause, strings.Join(refused, "; "))
	}
	needs := ""
	if len(names) > 0 {
		needs = "; it uses " + strings.Join(names, ", ") + ", which this host doesn't have"
	}
	return fmt.Errorf("%w: generation %d doesn't validate here%s: %w; install what it needs on this host, or replace it for every host with this host's config file using --adopt-config %d", ErrStoredConfigRefused, generation, needs, cause, generation)
}

// policyRefusal is a stored generation the daemon's policy doesn't allow.
func policyRefusal(generation uint64, cause error) error {
	return fmt.Errorf("stored generation %d breaks the daemon's policy, so it doesn't start: %w; to replace it, start once under a policy that allows it, apply a config that fits, then put the policy back", generation, cause)
}

// AuditActionConfigAdopted is the audit Action of an operator replacing shared
// state's stored generation with one host's config file.
const AuditActionConfigAdopted = "core.config.adopted"

// OpenHeldConfig is OpenConfig for shared state. A generation that doesn't
// validate may mean only that this host lacks something the others have, so it
// is never replaced from this host's file unless accept names that generation.
// refused describes installed packages this host won't run (RefuseStoredConfig).
func OpenHeldConfig(ctx context.Context, store storage.Store, now func() time.Time, registry config.Registry, path string, def []byte, accept uint64, refused []string) (*ConfigService, error) {
	return openConfigChecked(ctx, store, now, registry, path, def, &heldConfig{accept: accept, refused: refused})
}

// heldConfig is how a daemon on shared state opens the stored generation.
type heldConfig struct {
	// accept is the generation an operator accepts replacing, 0 for none.
	accept  uint64
	refused []string
}

// OpenConfig restores the committed generation. With no activation record it
// validates the config file and activates it as generation 1; with no config
// file either, it activates def instead when given (the default
// configuration), writing no file: the first apply writes it. Disk edits alone
// never change an already committed generation that still validates; one that
// no longer does is replaced by the config file when the file validates, so
// the daemon can always start once the file is fixed. The exceptions are a
// generation the daemon's policy refuses, which doesn't start at all, and
// shared state (OpenHeldConfig).
func OpenConfig(ctx context.Context, store storage.Store, now func() time.Time, registry config.Registry, path string, def []byte) (*ConfigService, error) {
	return openConfigChecked(ctx, store, now, registry, path, def, nil)
}

func openConfigChecked(ctx context.Context, store storage.Store, now func() time.Time, registry config.Registry, path string, def []byte, held *heldConfig) (*ConfigService, error) {
	c, err := openConfig(ctx, store, now, registry, path, def, held)
	if err != nil {
		return nil, err
	}
	if err = c.dropImplicitAdmissionLimit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// openConfig opens the committed generation. held is set on shared state.
func openConfig(ctx context.Context, store storage.Store, now func() time.Time, registry config.Registry, path string, def []byte, held *heldConfig) (*ConfigService, error) {
	c := &ConfigService{store: store, now: now, registry: registry, path: path, history: map[uint64]*config.Snapshot{}}
	if def != nil {
		if s, err := config.Validate(def, registry); err == nil {
			c.defaultHash = s.Hash
		}
	}
	generation, normalized, err := StoredGeneration(ctx, store, now)
	if err != nil {
		return nil, err
	}
	active := activation{Generation: generation}
	if active.Generation == 0 {
		data, err := config.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) && def != nil {
			data, err = def, nil
		}
		if err != nil {
			return nil, err
		}
		snapshot, err := config.ValidateFile(data, registry, c.includes(nil))
		if err != nil {
			return nil, err
		}
		if err = c.publish(ctx, snapshot, 0); err != nil {
			return nil, err
		}
		c.fileGeneration = c.generation
		return c, nil
	}
	snapshot, err := config.ValidateStored(normalized, registry)
	if config.PolicyRefused(err) {
		// Not adoption's case: this generation is what the daemon last ran,
		// and the policy says it doesn't start. Replacing it from a file on
		// disk would decide that for the operator.
		return nil, policyRefusal(active.Generation, err)
	}
	if err != nil {
		if held == nil {
			return c.adopt(ctx, active.Generation, normalized, err, nil)
		}
		if held.accept != active.Generation {
			return nil, RefuseStoredConfig(active.Generation, normalized, err, held.refused)
		}
		stored := active.Generation
		return c.adopt(ctx, stored, normalized, err, func(tx storage.Tx, generation uint64, hash string) error {
			_, err := tx.AppendAudit(domain.AuditRecord{Actor: "system:serve", Action: AuditActionConfigAdopted, Target: "config/generation", Outcome: domain.AuditOK, Detail: map[string]string{"replaced_generation": u64(stored), "generation": u64(generation), "hash": hash}})
			return err
		})
	}
	c.current, c.generation, c.fileGeneration = snapshot, active.Generation, active.Generation
	c.history[active.Generation] = snapshot
	return c, nil
}

// HasStoredGeneration reports whether a config generation was ever committed,
// so tx is state that has run rather than state just created.
func HasStoredGeneration(tx storage.Tx) (bool, error) {
	_, err := tx.Get(configNamespace, "active")
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// StoredGeneration reads the committed generation's number and normalized
// document without validating it: 0 and nil when none is committed. The
// daemon reads the operator's package grants from it (settings.extensions)
// to start the packages, which run before the configuration opens.
func StoredGeneration(ctx context.Context, store storage.Store, now func() time.Time) (uint64, []byte, error) {
	var generation uint64
	var normalized []byte
	err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		var err error
		generation, normalized, err = StoredGenerationIn(tx)
		return err
	})
	if err != nil {
		return 0, nil, err
	}
	return generation, normalized, nil
}

// StoredGenerationIn is StoredGeneration within a transaction or view.
func StoredGenerationIn(tx storage.Tx) (uint64, []byte, error) {
	var active activation
	b, err := tx.Get(configNamespace, "active")
	if errors.Is(err, storage.ErrNotFound) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	if err = json.Unmarshal(b, &active); err != nil {
		return 0, nil, err
	}
	normalized, err := tx.Get(configNamespace, "generation/"+u64(active.Generation))
	if err != nil {
		return 0, nil, err
	}
	return active.Generation, normalized, nil
}

// adopt publishes the config file over a stored generation that failed
// validation with cause. Its scopes still stand in for a folder that has gone.
// record, when given, writes an audit record in the publishing transaction.
func (c *ConfigService) adopt(ctx context.Context, stored uint64, normalized []byte, cause error, record Recorder) (*ConfigService, error) {
	fail := func(err error) (*ConfigService, error) {
		return nil, fmt.Errorf("stored generation %d no longer validates (%w); fix %s and restart to adopt it instead: %v", stored, cause, c.path, err)
	}
	data, err := config.ReadFile(c.path)
	if err != nil {
		return fail(err)
	}
	snapshot, err := config.ValidateFile(data, c.registry, c.includes(normalized))
	if err != nil {
		return fail(err)
	}
	c.mu.Lock()
	err = c.publishLocked(ctx, snapshot, stored, record)
	c.mu.Unlock()
	if err != nil {
		return fail(err)
	}
	c.fileGeneration = c.generation
	c.Adopted = &Adoption{Stored: stored, Cause: cause}
	return c, nil
}

// PublishStartConfig makes the config file at path the active generation of
// a stopped, just-restored store, in place of the generation the bundle was
// taken on, whose paths may belong to another machine. The file is validated
// as a config file (its includes must exist here), and publishing it follows
// the rules of any apply, so one that drops a scope with open Work is
// refused. The ledger records it.
func PublishStartConfig(ctx context.Context, store storage.Store, now func() time.Time, registry config.Registry, path string) (*config.Snapshot, error) {
	data, err := config.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &ConfigService{store: store, now: now, registry: registry, path: path, history: map[uint64]*config.Snapshot{}}
	snapshot, err := config.ValidateFile(data, registry, c.includes(nil))
	if err != nil {
		return nil, err
	}
	stored, _, err := StoredGeneration(ctx, store, now)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	err = c.publishLocked(ctx, snapshot, stored, func(tx storage.Tx, generation uint64, hash string) error {
		_, err := tx.AppendAudit(domain.AuditRecord{Actor: "system:restore", Action: "core.config.start_config", Outcome: domain.AuditOK, Detail: map[string]string{"replaced_generation": u64(stored), "generation": u64(generation), "hash": hash}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (c *ConfigService) Current() (*config.Snapshot, uint64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current, c.generation
}

// Holding runs fn with the current generation while no apply can commit,
// so what fn checks against it stays true until fn returns.
func (c *ConfigService) Holding(fn func(*config.Snapshot) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fn(c.current)
}

// Path is the config file the daemon saves applied generations to.
func (c *ConfigService) Path() string { return c.path }

// Registry returns the in-process registrations used for validation.
func (c *ConfigService) Registry() config.Registry { return c.registry }

// DiskStatus compares the config file with the committed generation without
// applying it: "current", "pending_edit" (valid but not applied), "invalid",
// "default" (no file, and the active generation is the default
// configuration), "missing" (no file, and a generation whose file has gone)
// or "unreadable". It never returns file
// content or diagnostics text.
func (c *ConfigService) DiskStatus() string {
	data, err := config.ReadFile(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		if current, _ := c.Current(); current != nil && c.defaultHash != "" && current.Hash == c.defaultHash {
			return "default"
		}
		return "missing"
	}
	if err != nil {
		return "unreadable"
	}
	snapshot, err := c.Validate(data)
	if err != nil {
		return "invalid"
	}
	current, _ := c.Current()
	if current == nil || snapshot.Hash != current.Hash {
		return "pending_edit"
	}
	return "current"
}

// Validate is side-effect free. It composes the config file's includes from
// their folders; a folder that has gone keeps the committed generation's
// scopes, disabled.
func (c *ConfigService) Validate(data []byte) (*config.Snapshot, error) {
	var previous []byte
	if current, _ := c.Current(); current != nil {
		previous = current.Normalized
	}
	return config.ValidateFile(data, c.registry, c.includes(previous))
}

// includes resolves include paths against the config file's directory, as
// the file applied through the API is saved there.
func (c *ConfigService) includes(previous []byte) config.Includes {
	return config.Includes{Base: filepath.Dir(c.path), Previous: previous}
}

// Apply fully revalidates, checks hash and generation, saves the file
// atomically (keeping last-good), then publishes the generation durably.
//
// hash is the candidate's FileHash and expected the generation it was
// based on. Fragment applies to submitted includes don't stale either: a
// generation published since expected by fragment applies alone still
// counts as expected (only a config file apply since then conflicts), and
// the candidate is composed again over the latest fragments before it is
// published, so it never undoes a team's apply.
//
// record, when given, writes the apply's audit record in the transaction
// that publishes the generation, given its number and hash.
func (c *ConfigService) Apply(ctx context.Context, data []byte, hash string, expected uint64, record Recorder) (uint64, string, error) {
	for range fragmentRetries {
		_, generation := c.Current()
		snapshot, err := c.Validate(data)
		if err != nil {
			return 0, "", err
		}
		if snapshot.FileHash != hash {
			return 0, "", ErrHashMismatch
		}
		c.mu.Lock()
		if expected < c.fileGeneration || expected > c.generation {
			c.mu.Unlock()
			return 0, "", ErrGenerationConflict
		}
		if c.generation != generation {
			// A fragment apply committed while this one validated: compose
			// again over it.
			c.mu.Unlock()
			continue
		}
		err = config.SaveAtomic(c.path, data)
		if err == nil {
			err = c.publishLocked(ctx, snapshot, c.generation, record)
		}
		if err == nil {
			c.fileGeneration = c.generation
			if c.OnApply != nil {
				c.OnApply(ctx, snapshot, c.generation)
			}
		}
		next := c.generation
		c.mu.Unlock()
		if err != nil {
			return 0, "", err
		}
		return next, snapshot.Hash, nil
	}
	return 0, "", ErrGenerationConflict
}

// Fragment reads the submitted include name from the committed generation.
func (c *ConfigService) Fragment(name string) (config.Fragment, uint64, error) {
	current, generation := c.Current()
	if current == nil {
		return config.Fragment{}, 0, config.ErrUnknownInclude
	}
	f, err := config.FragmentOf(current.Normalized, name)
	return f, generation, err
}

// ApplyFragment replaces the submitted include name's scopes with fragment
// and publishes the result as the next generation. It is checked against
// the include's own hash, not the generation, so applies to two includes
// never conflict: it composes against the committed generation (config.
// ApplyFragment), and if another apply commits meanwhile, composes again
// against that one. allowed is asked about the include's parent each time,
// since a config file apply may have moved it; the config file on disk is
// neither read nor written. record is as for Apply.
func (c *ConfigService) ApplyFragment(ctx context.Context, name string, fragment []byte, expectedHash string, allowed func(parent string) bool, record Recorder) (uint64, string, error) {
	for range fragmentRetries {
		current, generation := c.Current()
		if current == nil {
			return 0, "", config.ErrUnknownInclude
		}
		before, err := config.FragmentOf(current.Normalized, name)
		if err != nil {
			return 0, "", err
		}
		if !allowed(before.Parent) {
			return 0, "", ErrIncludeMoved
		}
		if before.Hash != expectedHash {
			return 0, "", ErrFragmentConflict
		}
		snapshot, err := config.ApplyFragment(current.Normalized, name, fragment, c.registry)
		if err != nil {
			return 0, "", err
		}
		after, err := config.FragmentOf(snapshot.Normalized, name)
		if err != nil {
			return 0, "", err
		}
		c.mu.Lock()
		if c.generation != generation {
			c.mu.Unlock()
			continue
		}
		if snapshot.Hash == current.Hash {
			// Nothing changed: no new generation.
			c.mu.Unlock()
			return generation, after.Hash, nil
		}
		err = c.publishLocked(ctx, snapshot, generation, record)
		if err == nil && c.OnApply != nil {
			c.OnApply(ctx, snapshot, c.generation)
		}
		next := c.generation
		c.mu.Unlock()
		if err != nil {
			return 0, "", err
		}
		return next, after.Hash, nil
	}
	return 0, "", ErrGenerationConflict
}

func (c *ConfigService) publish(ctx context.Context, s *config.Snapshot, expected uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.publishLocked(ctx, s, expected, nil)
}

// Recorder writes an apply's audit record in the transaction that
// publishes its generation, given the generation's number and hash.
type Recorder func(tx storage.Tx, generation uint64, hash string) error

func (c *ConfigService) publishLocked(ctx context.Context, s *config.Snapshot, expected uint64, record Recorder) error {
	next := expected + 1
	err := c.store.Transact(ctx, storage.Guard{Now: c.now()}, func(tx storage.Tx) error {
		var active activation
		if b, err := tx.Get(configNamespace, "active"); err == nil {
			if err = json.Unmarshal(b, &active); err != nil {
				return err
			}
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if active.Generation != expected {
			return ErrGenerationConflict
		}
		wanted := map[domain.ID]bool{}
		for _, scope := range s.DomainScopes() {
			wanted[scope.ID] = true
			old, err := tx.Scope(scope.ID)
			if err == nil {
				if old.Name == scope.Name && old.ParentID == scope.ParentID && reflect.DeepEqual(old.Bindings, scope.Bindings) && reflect.DeepEqual(old.Routes, scope.Routes) && reflect.DeepEqual(old.Config, scope.Config) && reflect.DeepEqual(old.SecretRefs, scope.SecretRefs) {
					continue
				}
				if old.ParentID != scope.ParentID {
					if err = scopeIdle(tx, scope.ID); err != nil {
						return err
					}
				}
				err = tx.PutScope(scope, old.Revision)
			} else if errors.Is(err, storage.ErrNotFound) {
				err = tx.PutScope(scope, 0)
			}
			if err != nil {
				return err
			}
		}
		existing, err := tx.ListScopes()
		if err != nil {
			return err
		}
		committed, err := committedScopes(tx, active.Generation)
		if err != nil {
			return err
		}
		for _, scope := range existing {
			// Removal requires drained nonterminal Work; history keeps its
			// record. A stored scope the committed generation already lacks
			// isn't being removed by this publish, so its Work doesn't block
			// it: the operator resolves that Work through the usual controls.
			if !wanted[scope.ID] && (committed == nil || committed[scope.ID]) {
				if err = scopeIdle(tx, scope.ID); err != nil {
					return err
				}
			}
		}
		if err = tx.Put(configNamespace, "generation/"+u64(next), s.Normalized); err != nil {
			return err
		}
		b, _ := json.Marshal(activation{Generation: next, Hash: s.Hash, ActivatedAt: c.now()})
		if err = tx.Put(configNamespace, "active", b); err != nil || record == nil {
			return err
		}
		return record(tx, next, s.Hash)
	})
	if err != nil {
		return err
	}
	c.current, c.generation = s, next
	c.history[next] = s
	return nil
}

// committedScopes is the set of scopes in the committed generation, or nil
// when that can't be read (none is committed, or its text no longer parses),
// so the caller treats every stored scope as possibly in it.
func committedScopes(tx storage.Tx, generation uint64) (map[domain.ID]bool, error) {
	if generation == 0 {
		return nil, nil
	}
	stored, err := tx.Get(configNamespace, "generation/"+u64(generation))
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	doc, err := config.Parse(stored)
	if err != nil {
		return nil, nil
	}
	scopes := make(map[domain.ID]bool, len(doc.Scopes))
	for _, scope := range doc.Scopes {
		scopes[domain.ID(scope.ID)] = true
	}
	return scopes, nil
}

// ScopeInUseError is a publish refused because a scope it would remove or
// reparent still has open Work.
type ScopeInUseError struct {
	Scope domain.ID
	Work  []domain.ID
}

// scopeInUseListed bounds how many Work IDs a refusal spells out.
const scopeInUseListed = 5

// Detail names the scope and its open Work, for the operator.
func (e *ScopeInUseError) Detail() string {
	shown := e.Work[:min(len(e.Work), scopeInUseListed)]
	ids := make([]string, len(shown))
	for i, id := range shown {
		ids[i] = string(id)
	}
	list := strings.Join(ids, ", ")
	if more := len(e.Work) - len(shown); more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	return fmt.Sprintf("scope %q has %d open Work (%s); finish or cancel it first", e.Scope, len(e.Work), list)
}

func (e *ScopeInUseError) Error() string { return ErrScopeInUse.Error() + ": " + e.Detail() }

// Is makes a ScopeInUseError match ErrScopeInUse.
func (e *ScopeInUseError) Is(target error) bool { return target == ErrScopeInUse }

func scopeIdle(tx storage.Tx, scope domain.ID) error {
	works, err := tx.ListWorks()
	if err != nil {
		return err
	}
	var open []domain.ID
	for _, w := range works {
		if w.ScopeID == scope && !w.State.Terminal() {
			open = append(open, w.ID)
		}
	}
	if len(open) > 0 {
		return &ScopeInUseError{Scope: scope, Work: open}
	}
	return nil
}

// Enabled reports the current effective enabled state; removed scopes are disabled.
func (c *ConfigService) Enabled(scope domain.ID) bool {
	s, _ := c.Current()
	e, ok := s.Scope(scope)
	return ok && e.Enabled
}

// Authorize is the runtime toolbelt ceiling: current config can only narrow.
func (c *ConfigService) Authorize(_ context.Context, check runtime.AuthorityCheck) error {
	s, _ := c.Current()
	if err := s.AuthorizeOperation(check.ScopeID, check.Binding, check.Operation); err != nil || check.WorkKind == "" {
		return err
	}
	return s.AuthorizeWorkKind(check.ScopeID, check.Binding, check.WorkKind)
}

// Refusal classifies a refused toolbelt call (runtime.ToolbeltOptions.Refusal)
// against the generation the call's execution is pinned to: an operation
// its binding's playbook declares is runtime.RefusalConfigDrift, the grant
// lagging what the playbook uses. The playbook was worked out when that
// generation was validated, so no executor is asked here.
func (c *ConfigService) Refusal(_ context.Context, check runtime.AuthorityCheck) string {
	if s := c.snapshotFor(check.Generation); s != nil && s.Declared(check.ScopeID, check.Binding, check.Operation) {
		return runtime.RefusalConfigDrift
	}
	return ""
}

func (c *ConfigService) snapshotFor(generation string) *config.Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, err := strconv.ParseUint(generation, 10, 64)
	if err != nil {
		return nil
	}
	if s := c.history[n]; s != nil {
		return s
	}
	var data []byte
	if c.store.Transact(context.Background(), storage.Guard{Now: c.now()}, func(tx storage.Tx) error {
		var e error
		data, e = tx.Get(configNamespace, "generation/"+u64(n))
		return e
	}) != nil {
		return nil
	}
	s, err := config.ValidateStored(data, c.registry)
	if err != nil {
		return nil
	}
	c.history[n] = s
	return s
}

// Grant returns the attempt's pinned binding operations, with those of the
// binding grants the attempt's Work opted into, intersected with the current
// ceiling, plus the registered emitted families and the pinned binding's
// connection.
//
// describe, when set, is what a kind of child Work in the scope is for (the
// route that binds it says), shown with the kinds the binding may ask for.
func (c *ConfigService) Grant(events func() []string, describe func(config.EffectiveScope, string) string) func(context.Context, runtime.AttemptState) (runtime.ToolbeltGrant, error) {
	return func(ctx context.Context, state runtime.AttemptState) (runtime.ToolbeltGrant, error) {
		grant := runtime.ToolbeltGrant{}
		pinned := c.snapshotFor(state.Generation)
		if pinned == nil {
			pinned, _ = c.Current()
		}
		scope, ok := pinned.Scope(state.ScopeID)
		if !ok {
			return grant, nil
		}
		binding, ok := scope.Binding(state.Binding.Name)
		if !ok {
			return grant, nil
		}
		grant.Connection = binding.Connection
		for _, kind := range binding.WorkKinds {
			if c.Authorize(ctx, runtime.AuthorityCheck{ScopeID: state.ScopeID, Binding: state.Binding.Name, Operation: extension.CreateWork, WorkKind: kind}) == nil {
				k := extension.WorkKind{Kind: kind}
				if describe != nil {
					k.Description = describe(*scope, kind)
				}
				grant.WorkKinds = append(grant.WorkKinds, k)
			}
		}
		ops := slices.Clone(binding.Operations)
		for _, g := range binding.Grants {
			if slices.Contains(state.Grants, g.Name) {
				ops = append(ops, g.Operations...)
			}
		}
		for _, op := range ops {
			if !slices.Contains(grant.Operations, op) && c.Authorize(ctx, runtime.AuthorityCheck{ScopeID: state.ScopeID, Binding: state.Binding.Name, Operation: op}) == nil {
				grant.Operations = append(grant.Operations, op)
			}
		}
		if events != nil {
			grant.EventTypes = events()
		}
		return grant, nil
	}
}

// Secrets resolves the attempt's pinned binding secret names, with those of
// the binding grants its Work took, that the current generation still grants,
// using current references (rotation affects future resolution). Nothing is
// cached across attempts or persisted.
func (c *ConfigService) Secrets(resolver *config.Resolver) func(context.Context, runtime.AttemptState) (map[string]string, error) {
	return func(ctx context.Context, state runtime.AttemptState) (map[string]string, error) {
		pinned := c.snapshotFor(state.Generation)
		current, _ := c.Current()
		if pinned == nil {
			pinned = current
		}
		ps, ok := pinned.Scope(state.ScopeID)
		cs, cok := current.Scope(state.ScopeID)
		if !ok || !cok || !cs.Enabled {
			return nil, config.ErrDenied
		}
		pb, ok := ps.Binding(state.Binding.Name)
		cb, cok := cs.Binding(state.Binding.Name)
		if !ok || !cok {
			return nil, config.ErrDenied
		}
		var names []string
		still := cb.SecretNamesWith(state.Grants)
		for _, name := range pb.SecretNamesWith(state.Grants) {
			if slices.Contains(still, name) {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			return map[string]string{}, nil
		}
		return resolver.ResolveNames(ctx, cs, names)
	}
}

// Resource returns the attempt's pinned binding resource descriptor (provider
// and opaque config), or nil when the binding declares none. A pinned attempt
// keeps its generation's descriptor; the binding must still exist in an
// enabled scope of the current generation.
// Sandbox is the attempt's binding sandbox (B09) in the current config, so a
// changed policy applies from the next segment; nil when it declares none.
func (c *ConfigService) Sandbox(state runtime.AttemptState) (*config.Sandbox, error) {
	current, _ := c.Current()
	cs, ok := current.Scope(state.ScopeID)
	if !ok || !cs.Enabled {
		return nil, config.ErrDenied
	}
	b, ok := cs.Binding(state.Binding.Name)
	if !ok {
		return nil, config.ErrDenied
	}
	return b.Sandbox, nil
}

// Container is the attempt's binding container in the current config, so a
// changed grant applies from the next segment; nil when it declares none.
func (c *ConfigService) Container(state runtime.AttemptState) (*config.Container, error) {
	current, _ := c.Current()
	cs, ok := current.Scope(state.ScopeID)
	if !ok || !cs.Enabled {
		return nil, config.ErrDenied
	}
	b, ok := cs.Binding(state.Binding.Name)
	if !ok {
		return nil, config.ErrDenied
	}
	return b.Container, nil
}

// Isolation names the scope whose authorization requires the attempt's
// scope to run confined in the current config (runtime.SchedulerOptions.
// Isolation), or "" when none does, so a requirement added after an attempt
// was queued or parked holds when it launches. A scope or binding the
// current config no longer has is config.ErrDenied.
func (c *ConfigService) Isolation(_ context.Context, state runtime.AttemptState) (domain.ID, error) {
	current, _ := c.Current()
	cs, ok := current.Scope(state.ScopeID)
	if !ok || !cs.Enabled {
		return "", config.ErrDenied
	}
	if _, ok := cs.Binding(state.Binding.Name); !ok {
		return "", config.ErrDenied
	}
	if cs.Authorization.Isolation == "" {
		return "", nil
	}
	return cs.IsolationScope, nil
}

// Connection is the connection the attempt's pinned binding names ("" for
// none), as Grant pins it into the toolbelt's authority.
func (c *ConfigService) Connection(state runtime.AttemptState) string {
	pinned := c.snapshotFor(state.Generation)
	if pinned == nil {
		pinned, _ = c.Current()
	}
	if s, ok := pinned.Scope(state.ScopeID); ok {
		if b, ok := s.Binding(state.Binding.Name); ok {
			return b.Connection
		}
	}
	return ""
}

func (c *ConfigService) Resource(state runtime.AttemptState) (*config.Resource, error) {
	pinned := c.snapshotFor(state.Generation)
	current, _ := c.Current()
	if pinned == nil {
		pinned = current
	}
	ps, ok := pinned.Scope(state.ScopeID)
	cs, cok := current.Scope(state.ScopeID)
	if !ok || !cok || !cs.Enabled {
		return nil, config.ErrDenied
	}
	pb, ok := ps.Binding(state.Binding.Name)
	if _, cok := cs.Binding(state.Binding.Name); !ok || !cok {
		return nil, config.ErrDenied
	}
	return pb.Resource, nil
}
