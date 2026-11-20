package runtime_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/storage"
)

type auditObserver struct {
	mu      sync.Mutex
	records []observability.Record
}

func (o *auditObserver) Observe(_ context.Context, r observability.Record) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.records = append(o.records, r)
	return nil
}

// Each audit record goes out as audit.recorded once it commits, with its
// Event's correlation when it points at one, and event.accepted says who
// caused an Event the host knows the actor of. A rolled-back append sends
// nothing.
func TestAuditRecordsGoOutAsTelemetry(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	observer := &auditObserver{}
	store, err := runtime.Instrument(db, runtime.TelemetryOptions{Observer: observer})
	if err != nil {
		t.Fatal(err)
	}
	err = store.Transact(ctx, storage.Guard{Now: ingressTime}, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "s", Name: "s"}, 0); err != nil {
			return err
		}
		e := domain.Event{ID: "manual/1", Type: "core.manual.cancel", Source: runtime.ManualSource, DeliveryID: "d", OccurredAt: ingressTime, ReceivedAt: ingressTime, Actor: "operator:alice", Via: "operator:acme-ui"}
		if _, _, err := runtime.AcceptInTx(tx, "s", e); err != nil {
			return err
		}
		if _, err := tx.AppendAudit(domain.AuditRecord{Actor: "operator:alice", Action: "core.manual.cancel", Outcome: domain.AuditOK, EventID: "manual/1"}); err != nil {
			return err
		}
		_, err := tx.AppendAudit(domain.AuditRecord{Action: "GET /v1/doctor", Outcome: "unauthenticated", Detail: map[string]string{"reason": "credential revoked"}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Transact(ctx, storage.Guard{Now: ingressTime}, func(tx storage.Tx) error {
		if _, err := tx.AppendAudit(domain.AuditRecord{Action: "POST /v1/auth/revoke", Outcome: domain.AuditOK}); err != nil {
			return err
		}
		return storage.ErrConflict
	})
	var audits []map[string]string
	for _, r := range observer.records {
		switch r.Name {
		case "event.accepted":
			if r.Attributes["event.actor"] != "operator:alice" || r.Attributes["event.via"] != "operator:acme-ui" {
				t.Errorf("event.accepted does not say who: %v", r.Attributes)
			}
		case "audit.recorded":
			audits = append(audits, r.Attributes)
			if r.Attributes["audit.event_id"] != "" && r.Correlation.EventID != "manual/1" {
				t.Errorf("audit.recorded for an Event lacks its correlation: %+v", r.Correlation)
			}
		}
	}
	if len(audits) != 2 {
		t.Fatalf("audit.recorded records: %v, want 2", audits)
	}
	if audits[0]["audit.sequence"] != "1" || audits[0]["audit.actor"] != "operator:alice" || audits[1]["audit.outcome"] != "unauthenticated" || audits[1]["audit.detail.reason"] != "credential revoked" || audits[1]["audit.prev"] != audits[0]["audit.hash"] {
		t.Fatalf("audit.recorded attributes: %v", audits)
	}
}
