package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// auditTable holds the audit ledger, one row per record under its sequence.
// It is created on every open, as the indexes are, so a database an earlier
// binary made gains it without a schema version change; an earlier binary
// that opens it leaves it alone and appends nothing, which leaves the chain
// whole. Every Transact holds the one write lock (BEGIN IMMEDIATE), so the
// next sequence, max+1, is never taken twice and the ledger has no gaps.
const auditTable = `CREATE TABLE IF NOT EXISTS audit (sequence INTEGER PRIMARY KEY CHECK(sequence>0), record BLOB NOT NULL)`

func (t *transaction) AppendAudit(r domain.AuditRecord) (domain.AuditRecord, error) {
	if !t.live {
		return domain.AuditRecord{}, storage.ErrInvalid
	}
	var last uint64
	var prev string
	var b []byte
	switch err := t.queryRow("SELECT record FROM audit ORDER BY sequence DESC LIMIT 1").Scan(&b); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return domain.AuditRecord{}, mapError(err)
	default:
		var tail domain.AuditRecord
		if err = json.Unmarshal(b, &tail); err != nil {
			return domain.AuditRecord{}, err
		}
		last, prev = tail.Sequence, tail.Hash
	}
	sealed, ok := domain.SealAudit(r, last, prev, t.now)
	if !ok {
		return domain.AuditRecord{}, storage.ErrInvalid
	}
	if b, err := encode(sealed); err != nil {
		return domain.AuditRecord{}, err
	} else if _, err = t.exec("INSERT INTO audit(sequence,record) VALUES(?,?)", sealed.Sequence, b); err != nil {
		return domain.AuditRecord{}, err
	}
	return sealed, nil
}

func (t *transaction) AuditRecords(after uint64, limit int) ([]domain.AuditRecord, error) {
	if !t.live {
		return nil, storage.ErrInvalid
	}
	if after > 1<<63-1 {
		return []domain.AuditRecord{}, nil
	}
	if limit <= 0 {
		limit = -1
	}
	return list[domain.AuditRecord](t, "SELECT record FROM audit WHERE sequence>? ORDER BY sequence LIMIT ?", after, limit)
}
