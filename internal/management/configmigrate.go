package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/storage"
)

// admissionDefaultMigrated marks a store whose committed generation has been
// through dropImplicitAdmissionLimit, so a 10 applied later is kept.
const admissionDefaultMigrated = "migrated/admission-limit-default"

// AuditActionAdmissionDefault is the audit Action of the generation the
// host publishes to drop an admission_limit default nobody chose.
const AuditActionAdmissionDefault = "core.config.admission_default_dropped"

// dropImplicitAdmissionLimit runs once per store, after OpenConfig has the
// committed generation. Until admission_limit was enforced (#706) it
// defaulted to 10 and every normalized generation wrote it, chosen or not;
// it had no effect, and config check said to remove it. It now has no
// default, so a committed generation carrying 10 would cap every scope
// after an upgrade. When it does, the host publishes the same config
// without it as the next generation, through the ordinary publish, with an
// audit record of its own (actor host:open-conduit, via upgrade) naming
// both generations and their hashes. Stored generations are never
// rewritten, so each one still matches the audit record that applied it.
// Any other value is what the operator wrote, and it stays.
func (c *ConfigService) dropImplicitAdmissionLimit(ctx context.Context) error {
	done := false
	if err := c.store.Transact(ctx, storage.Guard{Now: c.now()}, func(tx storage.Tx) error {
		_, err := tx.Get(configNamespace, admissionDefaultMigrated)
		done = err == nil
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	}); err != nil || done {
		return err
	}
	mark := func(tx storage.Tx) error { return tx.Put(configNamespace, admissionDefaultMigrated, []byte("true")) }
	current, generation := c.Current()
	var stripped []byte
	if current != nil && current.Settings.AdmissionLimit == 10 {
		stripped, _ = withoutImplicitAdmissionLimit(current.Normalized)
	}
	var next *config.Snapshot
	if stripped != nil {
		if s, err := config.Validate(stripped, c.registry); err == nil {
			next = s
		}
	}
	if next == nil {
		return c.store.Transact(ctx, storage.Guard{Now: c.now()}, mark)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.publishLocked(ctx, next, generation, func(tx storage.Tx, published uint64, hash string) error {
		r := domain.AuditRecord{Actor: "host:open-conduit", Via: "upgrade", Action: AuditActionAdmissionDefault, Target: "config/generation", Outcome: domain.AuditOK, Detail: map[string]string{
			"generation": u64(published), "hash": hash, "from_generation": u64(generation), "from_hash": current.Hash,
			"reason": "settings.admission_limit 10 was the unenforced default; it has no default now",
		}}
		if _, err := tx.AppendAudit(r); err != nil {
			return err
		}
		return mark(tx)
	})
}

// withoutImplicitAdmissionLimit is a generation's text without
// settings.admission_limit when that is 10, or nil.
func withoutImplicitAdmissionLimit(text []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(text))
	d.UseNumber()
	var doc map[string]any
	if err := d.Decode(&doc); err != nil {
		return nil, err
	}
	settings, _ := doc["settings"].(map[string]any)
	if n, ok := settings["admission_limit"].(json.Number); !ok || n.String() != "10" {
		return nil, nil
	}
	delete(settings, "admission_limit")
	return json.Marshal(doc)
}
