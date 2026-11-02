package bootstrap

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// act is Host.Act for the extension named kind (issue #364): it invokes an
// action for a person that extension's own surface identified. The caller's
// Kind is always kind, so an extension vouches only for identities on its
// own surface, and the provider authorises that caller as it would any
// other. The request key is the extension's, kept apart from every other
// caller's by its kind.
func (d *daemon) act(kind string) func(context.Context, extension.ActRequest) (extension.ActResult, error) {
	h := actHost{store: d.coord, now: d.now, providers: d.actions, poke: d.poke,
		submit: func(ctx context.Context, name string, p extension.ActionProvider, call extension.ActionCall, key string) (domain.AcceptedEvent, bool, string, error) {
			return d.current.Load().control.Act(ctx, name, p, call, key)
		}}
	return h.act(kind)
}

// actHost is what Host.Act needs of the daemon: the store, the action
// providers on a scope and the action path (runtime.Control.Act).
type actHost struct {
	store     storage.Store
	now       func() time.Time
	providers func(ctx context.Context, scope domain.ID) map[string]extension.ActionProvider
	submit    func(ctx context.Context, name string, p extension.ActionProvider, call extension.ActionCall, key string) (domain.AcceptedEvent, bool, string, error)
	poke      func()
}

func (h actHost) act(kind string) func(context.Context, extension.ActRequest) (extension.ActResult, error) {
	return func(ctx context.Context, r extension.ActRequest) (extension.ActResult, error) {
		if !actField(r.Caller) || !actField(r.Key) || r.Scope == "" || r.Action == "" || len(r.Input) > 4096 {
			return extension.ActResult{}, extension.ErrInvalid
		}
		var t extension.ActionTarget
		if err := h.store.Transact(ctx, storage.Guard{Now: h.now()}, func(tx storage.Tx) error {
			if r.Work != "" {
				w, err := tx.Work(r.Work)
				if err != nil {
					return err
				}
				if w.ScopeID != r.Scope {
					return storage.ErrNotFound
				}
				t.Work = &w
			}
			var err error
			t.Scope, err = tx.Scope(r.Scope)
			return err
		}); err != nil {
			return extension.ActResult{}, err
		}
		name, p, err := h.provider(ctx, t, r.Provider, r.Action)
		if err != nil {
			return extension.ActResult{}, err
		}
		call := extension.ActionCall{Target: t, Action: r.Action, Input: r.Input, Caller: extension.Caller{Kind: kind, ID: r.Caller}}
		accepted, added, msg, err := h.submit(ctx, name, p, call, kind+":"+r.Key)
		if err != nil {
			return extension.ActResult{}, err
		}
		if h.poke != nil {
			h.poke()
		}
		return extension.ActResult{Event: accepted, Added: added, Message: msg}, nil
	}
}

// provider is the named provider on t, or with no name the one provider
// that offers action on t now.
func (h actHost) provider(ctx context.Context, t extension.ActionTarget, name, action string) (string, extension.ActionProvider, error) {
	providers := h.providers(ctx, t.Scope.ID)
	if name != "" {
		p, ok := providers[name]
		if !ok {
			return "", nil, &extension.OperationError{Code: extension.CodeNoAction, Message: "no such action provider on this target"}
		}
		return name, p, nil
	}
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	sort.Strings(names)
	var found []string
	for _, n := range names {
		list, err := providers[n].Actions(ctx, t)
		if err != nil {
			continue
		}
		for _, a := range list {
			if a.ID == action {
				found = append(found, n)
				break
			}
		}
	}
	switch len(found) {
	case 0:
		return "", nil, &extension.OperationError{Code: extension.CodeNoAction, Message: "nothing here offers " + action + " now"}
	case 1:
		return found[0], providers[found[0]], nil
	}
	return "", nil, &extension.OperationError{Code: "ambiguous", Message: action + " is offered by " + strings.Join(found, ", ") + "; name one"}
}

// actField is a caller ID or key: one short line of printable text.
func actField(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}
