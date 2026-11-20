package domain

import (
	"errors"
	"testing"
	"time"
)

// A chain sealed record by record verifies, and a copy with any record
// rewritten, dropped or reordered does not.
func TestVerifyAuditChainCatchesRewrites(t *testing.T) {
	at := time.Date(2026, 11, 20, 12, 0, 0, 5, time.UTC)
	var chain []AuditRecord
	var last uint64
	var prev string
	for _, action := range []string{"POST /v1/config/apply", "POST /v1/auth/revoke", "core.manual.cancel"} {
		r, ok := SealAudit(AuditRecord{Actor: "operator:alice", Action: action, Outcome: AuditOK, Detail: map[string]string{"k": "v"}}, last, prev, at)
		if !ok {
			t.Fatal("seal refused a whole record")
		}
		chain = append(chain, r)
		last, prev = r.Sequence, r.Hash
	}
	if seq, hash, err := VerifyAuditChain(0, "", chain); err != nil || seq != 3 || hash != prev {
		t.Fatalf("sealed chain: %d %s %v", seq, hash, err)
	}
	if _, ok := SealAudit(AuditRecord{Action: "x"}, 0, "", at); ok {
		t.Fatal("sealed a record without an outcome")
	}
	rewritten := append([]AuditRecord(nil), chain...)
	rewritten[1].Actor = "operator:mallory"
	dropped := []AuditRecord{chain[0], chain[2]}
	reordered := []AuditRecord{chain[1], chain[0], chain[2]}
	detail := append([]AuditRecord(nil), chain...)
	detail[0].Detail = map[string]string{"k": "w"}
	for name, c := range map[string][]AuditRecord{"rewritten": rewritten, "dropped": dropped, "reordered": reordered, "detail changed": detail} {
		if _, _, err := VerifyAuditChain(0, "", c); !errors.Is(err, ErrAuditChain) {
			t.Errorf("%s: got %v, want ErrAuditChain", name, err)
		}
	}
	// A page continues from the last record checked, not from the start.
	if _, _, err := VerifyAuditChain(1, chain[0].Hash, chain[1:]); err != nil {
		t.Fatalf("a later page: %v", err)
	}
	if _, _, err := VerifyAuditChain(1, "other", chain[1:]); !errors.Is(err, ErrAuditChain) {
		t.Fatalf("a page after another hash: %v", err)
	}
}
