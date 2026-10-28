package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

var (
	ErrIngressBusy     = errors.New("ingress scan already running")
	ErrScanLeaseLost   = errors.New("ingress scan lease expired or superseded")
	ErrIngressProvider = errors.New("ingress provider failed")
	// ErrIngressNamespaceOwned is a source namespace already stored for
	// another Scope; ErrIngressNameTaken is a registration name already
	// stored for another source. Both also match extension.ErrConflict.
	ErrIngressNamespaceOwned = errors.New("ingress namespace belongs to another scope")
	ErrIngressNameTaken      = errors.New("ingress name belongs to another source")
)

type IngressOptions struct {
	ScanLease, PageTimeout                   time.Duration
	MaxPageItems, MaxScanItems, MaxScanPages int
	MaxPageBytes, MaxScanBytes               int
}
type ingressRegistration struct {
	name     string
	source   extension.Source
	adapter  extension.Ingress
	interval time.Duration
	families map[string]extension.EventFamily
}
type Ingress struct {
	store         storage.Store
	now           func() time.Time
	options       IngressOptions
	mu            sync.RWMutex
	registrations map[string]ingressRegistration
}

func NewIngress(store storage.Store, now func() time.Time, options IngressOptions) (*Ingress, error) {
	if store == nil || now == nil {
		return nil, storage.ErrInvalid
	}
	if options.ScanLease == 0 {
		options.ScanLease = time.Minute
	}
	if options.PageTimeout == 0 {
		options.PageTimeout = 30 * time.Second
	}
	if options.MaxPageItems == 0 {
		options.MaxPageItems = 1000
	}
	if options.MaxScanItems == 0 {
		options.MaxScanItems = 10000
	}
	if options.MaxScanPages == 0 {
		options.MaxScanPages = 1000
	}
	if options.MaxPageBytes == 0 {
		options.MaxPageBytes = 8 << 20
	}
	if options.MaxScanBytes == 0 {
		options.MaxScanBytes = 32 << 20
	}
	if options.ScanLease <= 0 || options.PageTimeout <= 0 || options.PageTimeout >= options.ScanLease || options.MaxPageItems < 1 || options.MaxPageItems > 10000 || options.MaxScanItems < options.MaxPageItems || options.MaxScanItems > 1000000 || options.MaxScanPages < 1 || options.MaxScanPages > 10000 || options.MaxPageBytes < 1 || options.MaxPageBytes > 64<<20 || options.MaxScanBytes < options.MaxPageBytes || options.MaxScanBytes > 256<<20 {
		return nil, storage.ErrInvalid
	}
	return &Ingress{store: store, now: now, options: options, registrations: map[string]ingressRegistration{}}, nil
}
func sourceNamespace(source extension.Source) string {
	return "core.ingress." + identity(source.Namespace)
}

type ingressState struct {
	Name                                       string
	Source                                     extension.Source
	Checkpoint, ScanID, Cursor, PageCheckpoint string
	SeenCursors                                []string
	NextDue, LeaseUntil                        time.Time
	LeaseToken                                 string
	Failures                                   uint32
	LastError                                  string
	LastComplete                               time.Time
	// Requests counts operator reconcile requests; GenerationRequests is its
	// value when the current generation's first page was claimed. A request
	// that arrives after that claim (a page in flight, or a generation part
	// way through its pages) may not be reflected in the generation, so its
	// completion leaves the source due instead of waiting a full interval.
	Requests, GenerationRequests uint64
}

func (i *Ingress) Register(ctx context.Context, name string, source extension.Source, adapter extension.Ingress, interval time.Duration) error {
	if len(name) > 128 || len(source.Namespace) > 1024 || name == "" || strings.TrimSpace(name) != name || source.Namespace == "" || strings.HasPrefix(source.Namespace, "core.") || source.ScopeID == "" || adapter == nil || interval < time.Second || interval > 365*24*time.Hour {
		return storage.ErrInvalid
	}
	families := map[string]extension.EventFamily{}
	for _, family := range adapter.Spec().Families {
		if !validType(family.Type) || strings.HasPrefix(family.Type, "core.") || !family.ReDerivable && strings.TrimSpace(family.Loss) == "" {
			return extension.ErrInvalid
		}
		if _, ok := families[family.Type]; ok {
			return extension.ErrConflict
		}
		families[family.Type] = family
	}
	if len(families) == 0 {
		return extension.ErrInvalid
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, ok := i.registrations[name]; ok {
		return extension.ErrConflict
	}
	for _, r := range i.registrations {
		if r.source.Namespace == source.Namespace {
			return extension.ErrConflict
		}
	}
	err := i.store.Transact(ctx, storage.Guard{Now: i.now()}, func(tx storage.Tx) error {
		if _, err := tx.Scope(source.ScopeID); err != nil {
			return err
		}
		var registeredSource extension.Source
		nameErr := getJSON(tx, "core.ingress.registration", identity(name), &registeredSource)
		if nameErr == nil && registeredSource != source {
			return fmt.Errorf("%w: %w: %s is stored for namespace %s in scope %s", ErrIngressNameTaken, extension.ErrConflict, name, registeredSource.Namespace, registeredSource.ScopeID)
		}
		if nameErr != nil && !errors.Is(nameErr, storage.ErrNotFound) {
			return nameErr
		}
		if errors.Is(nameErr, storage.ErrNotFound) {
			if err := putJSON(tx, "core.ingress.registration", identity(name), source); err != nil {
				return err
			}
		}
		var state ingressState
		err := getJSON(tx, sourceNamespace(source), "state", &state)
		if err == nil {
			if state.Source != source {
				return fmt.Errorf("%w: %w: %s is stored for scope %s as %s", ErrIngressNamespaceOwned, extension.ErrConflict, source.Namespace, state.Source.ScopeID, state.Name)
			}
			if state.Name == name {
				return nil
			}
			// The same namespace and Scope under a new name is a renamed
			// configuration entry: the stored source, checkpoint and history
			// move to the new name, and the old name is released.
			if err := tx.Delete("core.ingress.registration", identity(state.Name)); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			state.Name = name
			return putJSON(tx, sourceNamespace(source), "state", state)
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		return putJSON(tx, sourceNamespace(source), "state", ingressState{Name: name, Source: source, NextDue: i.now()})
	})
	if err != nil {
		return err
	}
	i.registrations[name] = ingressRegistration{name: name, source: source, adapter: adapter, interval: interval, families: families}
	return nil
}
func validType(s string) bool {
	if !strings.Contains(s, ".") || strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	for _, p := range strings.Split(s, ".") {
		if p == "" {
			return false
		}
	}
	return true
}
func (i *Ingress) registration(name string) (ingressRegistration, error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	r, ok := i.registrations[name]
	if !ok {
		return r, storage.ErrNotFound
	}
	return r, nil
}
func validateEvent(r ingressRegistration, event domain.Event, rederivable bool) error {
	family, ok := r.families[event.Type]
	if !ok || family.ReDerivable != rederivable || event.Source != r.source.Namespace || event.ID == "" || event.DeliveryID == "" || event.CausationID != "" || strings.HasPrefix(event.Type, "core.") {
		return extension.ErrInvalid
	}
	return nil
}

// AdmitsTrace checks a hypothetical Event against the registered source it
// names: known is false when no registered source has e.Source as its
// namespace, and an error says the source belongs to another scope, where
// every path accepts its Events. The type is not checked against the
// source's families: the same namespace also carries Events its extension
// derives or fires from timers (a schedule's slots).
func (i *Ingress) AdmitsTrace(scope domain.ID, e domain.Event) (known bool, err error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	for _, r := range i.registrations {
		if r.source.Namespace != e.Source {
			continue
		}
		if r.source.ScopeID != scope {
			return true, fmt.Errorf("source %s belongs to scope %s", e.Source, r.source.ScopeID)
		}
		return true, nil
	}
	return false, nil
}

func (i *Ingress) Push(ctx context.Context, name string, delivery extension.Delivery) ([]domain.AcceptedEvent, error) {
	r, err := i.registration(name)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, i.options.PageTimeout)
	defer cancel()
	result, err := r.adapter.Push(ctx, r.source, delivery)
	if err != nil {
		return nil, ErrIngressProvider
	}
	if len(result.Observations)+len(result.Events) > i.options.MaxPageItems {
		return nil, extension.ErrInvalid
	}
	encoded, encodeErr := json.Marshal(result)
	if encodeErr != nil || len(encoded) > i.options.MaxPageBytes {
		return nil, extension.ErrInvalid
	}
	accepted := []domain.AcceptedEvent{}
	err = i.store.Transact(ctx, storage.Guard{Now: i.now()}, func(tx storage.Tx) error {
		for _, observation := range result.Observations {
			if err := validateObservation(tx, r, observation); err != nil {
				return err
			}
			event, _, err := AcceptInTx(tx, r.source.ScopeID, observation.Event)
			if err != nil {
				return err
			}
			accepted = append(accepted, event)
		}
		for _, event := range result.Events {
			if err := validateEvent(r, event, false); err != nil {
				return err
			}
			a, _, err := AcceptInTx(tx, r.source.ScopeID, event)
			if err != nil {
				return err
			}
			accepted = append(accepted, a)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return accepted, nil
}

// IngressStatus is safe configuration/health output. It contains declarations
// and progress only, never credentials, delivery bodies or opaque observations.
type IngressStatus struct {
	Name                       string
	Source                     extension.Source
	Families                   []extension.EventFamily
	Checkpoint, ScanID, Cursor string
	NextDue                    time.Time
	LastError                  string
	LastComplete               time.Time
	LimitedRecovery            bool
	Interval                   time.Duration
	Failures                   uint32
}

func (i *Ingress) Inspect(ctx context.Context) ([]IngressStatus, error) {
	i.mu.RLock()
	regs := make([]ingressRegistration, 0, len(i.registrations))
	for _, r := range i.registrations {
		regs = append(regs, r)
	}
	i.mu.RUnlock()
	sort.Slice(regs, func(a, b int) bool { return regs[a].name < regs[b].name })
	out := []IngressStatus{}
	err := i.store.Transact(ctx, storage.Guard{Now: i.now()}, func(tx storage.Tx) error {
		for _, r := range regs {
			var state ingressState
			if err := getJSON(tx, sourceNamespace(r.source), "state", &state); err != nil {
				return err
			}
			status := IngressStatus{Name: r.name, Source: r.source, Checkpoint: state.Checkpoint, ScanID: state.ScanID, Cursor: state.Cursor, NextDue: state.NextDue, LastError: state.LastError, LastComplete: state.LastComplete, Interval: r.interval, Failures: state.Failures}
			for _, family := range r.families {
				status.Families = append(status.Families, family)
				if !family.ReDerivable {
					status.LimitedRecovery = true
				}
			}
			sort.Slice(status.Families, func(a, b int) bool { return status.Families[a].Type < status.Families[b].Type })
			out = append(out, status)
		}
		return nil
	})
	return out, err
}
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func scanError(code string) error { return fmt.Errorf("%w: %s", extension.ErrInvalid, code) }
