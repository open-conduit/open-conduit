package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/storage"
)

// admissionDefaultMigrated marks a store whose generations have been
// through dropImplicitAdmissionLimit.
const admissionDefaultMigrated = "migrated/admission-limit-default"

// dropImplicitAdmissionLimit rewrites, once per store, every stored
// generation that carries settings.admission_limit 10 without it. Until
// admission_limit was enforced (#706) it defaulted to 10 and every
// normalized generation wrote it, chosen or not; it had no effect, and
// config check said to remove it. It now has no default, so the 10 an
// operator never chose must not cap their scopes after an upgrade. Any
// other value is what the operator wrote, and it stays. A rewritten
// generation that validates is stored normalized, and the activation takes
// its new hash.
func dropImplicitAdmissionLimit(ctx context.Context, store storage.Store, now func() time.Time, registry config.Registry) error {
	return store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		if _, err := tx.Get(configNamespace, admissionDefaultMigrated); err == nil || !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		var active activation
		activeRaw, err := tx.Get(configNamespace, "active")
		switch {
		case err == nil:
			if err = json.Unmarshal(activeRaw, &active); err != nil {
				return err
			}
		case !errors.Is(err, storage.ErrNotFound):
			return err
		}
		rows, err := tx.List(configNamespace, "generation/")
		if err != nil {
			return err
		}
		for _, row := range rows {
			if !strings.HasPrefix(row.Key, "generation/") {
				continue
			}
			text, changed, err := withoutImplicitAdmissionLimit(row.Value)
			if err != nil || !changed {
				// A generation that does not parse is left for OpenConfig
				// to adopt over, as before.
				continue
			}
			hash := ""
			if s, err := config.Validate(text, registry); err == nil {
				text, hash = s.Normalized, s.Hash
			}
			if err = tx.Put(configNamespace, row.Key, text); err != nil {
				return err
			}
			if hash != "" && row.Key == "generation/"+u64(active.Generation) {
				active.Hash = hash
				b, _ := json.Marshal(active)
				if err = tx.Put(configNamespace, "active", b); err != nil {
					return err
				}
			}
		}
		return tx.Put(configNamespace, admissionDefaultMigrated, []byte("true"))
	})
}

// withoutImplicitAdmissionLimit is a generation's text without
// settings.admission_limit when that is 10.
func withoutImplicitAdmissionLimit(text []byte) ([]byte, bool, error) {
	d := json.NewDecoder(bytes.NewReader(text))
	d.UseNumber()
	var doc map[string]any
	if err := d.Decode(&doc); err != nil {
		return nil, false, err
	}
	settings, _ := doc["settings"].(map[string]any)
	if n, ok := settings["admission_limit"].(json.Number); !ok || n.String() != "10" {
		return text, false, nil
	}
	delete(settings, "admission_limit")
	out, err := json.Marshal(doc)
	return out, err == nil, err
}
