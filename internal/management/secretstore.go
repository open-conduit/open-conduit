package management

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/storage"
)

// secretsNamespace indexes the names stored in the keyring through the API,
// which the keyring cannot list without reading every value. It holds names
// and times only, never a value.
const secretsNamespace = "core.management.secrets"

type keyringEntry struct {
	StoredAt time.Time `json:"stored_at"`
	StoredBy string    `json:"stored_by"`
}

// wireSecret is one stored secret as GET /secrets lists it: never its value.
type wireSecret struct {
	Name         string   `json:"name"`
	Store        string   `json:"store"`
	Reference    string   `json:"reference"`
	Resolves     bool     `json:"resolves"`
	Failure      string   `json:"failure,omitempty"`
	ReferencedBy []string `json:"referenced_by"`
}

func (s *Server) secretStore() (*config.SecretStore, error) {
	if s.o.Secrets == nil {
		return nil, fail(503, "secrets_unavailable", "this daemon has no secret store")
	}
	return s.o.Secrets, nil
}

// listSecrets is GET /secrets: every name the daemon's secret store holds
// that it can know of (the secrets file's names, the names stored in the
// keyring through this API, and every keyring: or file: reference the active
// config makes), with where it is stored, whether it resolves now and which
// references in the active config name it. Values are read only to check
// they resolve, and never returned.
func (s *Server) listSecrets(c *call) (int, any, error) {
	var err error
	defer func() { s.observeOperator(c, "operator.secrets_listed", "", "", err) }()
	store, err := s.secretStore()
	if err != nil {
		return 0, nil, err
	}
	type key struct{ store, name string }
	found := map[key]bool{}
	names, err := store.FileNames()
	if err != nil {
		err = secretStoreError(err)
		return 0, nil, err
	}
	for _, n := range names {
		found[key{config.StoreFile, n}] = true
	}
	if err = s.read(c.ctx, func(tx storage.Tx) error {
		entries, e := tx.List(secretsNamespace, config.StoreKeyring+"/")
		for _, en := range entries {
			found[key{config.StoreKeyring, strings.TrimPrefix(en.Key, config.StoreKeyring+"/")}] = true
		}
		return e
	}); err != nil {
		return 0, nil, err
	}
	snapshot, _ := s.o.Config.Current()
	refs := secretReferences(snapshot)
	for ref := range refs {
		scheme, name, _ := strings.Cut(ref, ":")
		if scheme == config.StoreKeyring || scheme == config.StoreFile {
			found[key{scheme, name}] = true
		}
	}
	out := make([]wireSecret, 0, len(found))
	for k := range found {
		ref := k.store + ":" + k.name
		w := wireSecret{Name: k.name, Store: k.store, Reference: ref, ReferencedBy: refs[ref]}
		if w.ReferencedBy == nil {
			w.ReferencedBy = []string{}
		}
		if s.o.Resolver == nil {
			w.Failure = "no_resolver"
		} else if _, rerr := s.o.Resolver.Resolve(c.ctx, ref); rerr != nil {
			w.Failure = config.SecretCode(rerr)
		} else {
			w.Resolves = true
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Store < out[j].Store
	})
	return 200, map[string]any{"secrets": out}, nil
}

// putSecret is PUT /secrets/{name}: store or replace the value in the body
// in the keyring (preferred, falling back to the secrets file) or the store
// the body names, and answer with the reference a config uses to read it.
// The value is never logged, recorded or returned, and the request is not
// kept for replay: storing the same value again has the same effect.
func (s *Server) putSecret(c *call) (int, any, error) {
	name := c.params[0]
	var in struct {
		Value *string `json:"value"`
		Store string  `json:"store"`
	}
	var err error
	defer func() {
		s.observeOperator(c, "operator.secret_stored", name, storeOf(in.Store), err)
	}()
	store, err := s.secretStore()
	if err != nil {
		return 0, nil, err
	}
	if err = c.decode(&in); err != nil {
		return 0, nil, err
	}
	if in.Value == nil {
		err = fail(400, "malformed", "value is required")
		return 0, nil, err
	}
	if in.Store != "" && in.Store != config.StoreKeyring && in.Store != config.StoreFile {
		err = fail(422, "invalid_store", "store is keyring or file, or omitted for the keyring with the secrets file as fallback")
		return 0, nil, err
	}
	ref, fellBack, err := store.Set(name, *in.Value, in.Store)
	if err != nil {
		err = secretStoreError(err)
		return 0, nil, err
	}
	stored, _, _ := strings.Cut(ref, ":")
	in.Store = stored
	if stored == config.StoreKeyring {
		b, _ := json.Marshal(keyringEntry{StoredAt: s.o.Now().UTC(), StoredBy: c.who.actor()})
		if err = s.o.Store.Transact(c.ctx, storage.Guard{Now: s.o.Now()}, func(tx storage.Tx) error {
			return tx.Put(secretsNamespace, config.StoreKeyring+"/"+name, b)
		}); err != nil {
			return 0, nil, err
		}
	}
	return 200, map[string]any{"name": name, "store": stored, "reference": ref, "fell_back": fellBack}, nil
}

// deleteSecret is DELETE /secrets/{name}: remove the name from the keyring
// and the secrets file, refused while a reference in the active config
// reads it from either. No apply commits between the check and the removal.
func (s *Server) deleteSecret(c *call) (int, any, error) {
	name := c.params[0]
	var err error
	defer func() { s.observeOperator(c, "operator.secret_removed", name, "", err) }()
	store, err := s.secretStore()
	if err != nil {
		return 0, nil, err
	}
	if len(c.body) > 0 {
		var in struct{}
		if err = c.decode(&in); err != nil {
			return 0, nil, err
		}
	}
	if !config.ValidSecretName(name) {
		err = secretStoreError(config.ErrSecretName)
		return 0, nil, err
	}
	err = s.o.Config.Holding(func(snapshot *config.Snapshot) error {
		refs := secretReferences(snapshot)
		var using []config.Diagnostic
		for _, ref := range []string{config.StoreKeyring + ":" + name, config.StoreFile + ":" + name} {
			for _, p := range refs[ref] {
				using = append(using, config.Diagnostic{Path: p, Code: "secret_in_use", Message: "the active config reads " + ref + " here"})
			}
		}
		if len(using) > 0 {
			return &apiError{409, "secret_in_use", "the active config refers to this secret; apply a config without those references first", using}
		}
		indexed := false
		if e := s.read(c.ctx, func(tx storage.Tx) error {
			_, e := tx.Get(secretsNamespace, config.StoreKeyring+"/"+name)
			if errors.Is(e, storage.ErrNotFound) {
				return nil
			}
			indexed = e == nil
			return e
		}); e != nil {
			return e
		}
		// A keyring this daemon cannot use holds nothing to remove, unless
		// the API stored the name there.
		if e := store.Remove(name, config.StoreKeyring); e != nil && indexed {
			return secretStoreError(e)
		}
		if e := store.Remove(name, config.StoreFile); e != nil {
			return secretStoreError(e)
		}
		return s.o.Store.Transact(c.ctx, storage.Guard{Now: s.o.Now()}, func(tx storage.Tx) error {
			e := tx.Delete(secretsNamespace, config.StoreKeyring+"/"+name)
			if errors.Is(e, storage.ErrNotFound) {
				return nil
			}
			return e
		})
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"name": name, "removed": true}, nil
}

func storeOf(store string) string {
	if store == "" {
		return "default"
	}
	return store
}

func secretStoreError(err error) error {
	switch {
	case errors.Is(err, config.ErrSecretName):
		return fail(422, "invalid_secret_name", "secret names match [a-z][a-z0-9_-]* (at most 63 characters)")
	case errors.Is(err, config.ErrSecretValue):
		return fail(422, "invalid_secret_value", "a secret value is 1 byte to 64 KiB")
	case errors.Is(err, config.ErrKeyringRefused):
		return fail(503, "keyring_refused", "the platform keyring refused the request; store the secret in the file instead (store: file)")
	case errors.Is(err, config.ErrSecretsFile):
		return fail(503, "secrets_file_unavailable", "the secrets file is unreadable, not owner-only or not a JSON object of strings")
	}
	return err
}

// secretReferences maps each secret reference the generation's document
// makes, in any secret_refs object, to the JSON pointers that make it.
func secretReferences(snapshot *config.Snapshot) map[string][]string {
	out := map[string][]string{}
	if snapshot == nil {
		return out
	}
	var root any
	if json.Unmarshal(snapshot.Normalized, &root) != nil {
		return out
	}
	esc := strings.NewReplacer("~", "~0", "/", "~1")
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, child := range v {
				p := path + "/" + esc.Replace(k)
				if refs, ok := child.(map[string]any); ok && k == "secret_refs" {
					for name, ref := range refs {
						if ref, ok := ref.(string); ok && ref != "" {
							out[ref] = append(out[ref], p+"/"+esc.Replace(name))
						}
					}
					continue
				}
				walk(p, child)
			}
		case []any:
			for i, child := range v {
				walk(path+"/"+strconv.Itoa(i), child)
			}
		}
	}
	walk("", root)
	for _, ps := range out {
		sort.Strings(ps)
	}
	return out
}

// observeOperator records one administrative call in the operator trail:
// who made it, on which name, and how it ended (ok, or the refusal's code).
// It carries no value, and is anchored by the operator, not a scope.
func (s *Server) observeOperator(c *call, name, target, store string, err error) {
	if s.o.Observer == nil {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = mapError(err).code
	}
	at := s.o.Now()
	trace := digest("operator", c.who.actor(), name, target, at.UTC().Format(time.RFC3339Nano))
	attrs := map[string]string{"operator.actor": c.who.actor(), "operator.outcome": outcome, "record_id": trace}
	if target != "" {
		attrs["operator.target"] = target
	}
	if store != "" {
		attrs["operator.store"] = store
	}
	rec := observability.Record{Name: name, At: at, Correlation: observability.Correlation{TraceID: trace}, Attributes: attrs}
	if observability.Validate(rec) != nil {
		return
	}
	_ = s.o.Observer.Observe(context.WithoutCancel(c.ctx), rec)
}
