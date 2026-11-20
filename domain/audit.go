package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// AuditOK is the Outcome of an administrative call that did what it asked.
// Any other Outcome is the code it was refused or failed with
// ("forbidden_role", "unauthenticated", "invalid_config").
const AuditOK = "ok"

// AuditRecord is one entry in the audit ledger: an administrative call, who
// made it, what it acted on and how it ended. The ledger is durable and
// append-only. Storage numbers its records 1, 2, 3 with no gaps, and chains
// them: each record's Prev is the Hash of the one before it, and its Hash is
// Digest over everything else. A copy held elsewhere (a SIEM) can then tell
// a missing, reordered or rewritten record from the chain alone, though the
// local store can be rewritten by whoever owns its files.
type AuditRecord struct {
	// Sequence, At, Prev and Hash are storage's (Tx.AppendAudit); a
	// writer's values are ignored.
	Sequence uint64
	At       time.Time
	// Actor is who made the call, kind:id ("operator:alice"), as the host
	// authenticated it; empty when it authenticated no one. Via is the
	// client the Actor acted through, as on an Event.
	Actor string `json:",omitempty"`
	Via   string `json:",omitempty"`
	// Action is what was asked: the management route ("POST
	// /v1/config/apply") or, for a control or action that made an Event,
	// that Event's type.
	Action string
	// Target is what the call acted on, kind/id ("work/w-1",
	// "secret/github-token", "credential/tok_1"); empty for the daemon as
	// a whole.
	Target string `json:",omitempty"`
	// Outcome is AuditOK or the refusal's code.
	Outcome string
	// EventID is the Event the call made, when it made one, which holds
	// the rest of what it did.
	EventID ID `json:",omitempty"`
	// Detail holds a few further facts that are never secret (the
	// generation a config apply made, the role a refusal needed).
	Detail map[string]string `json:",omitempty"`
	Prev   string            `json:",omitempty"`
	Hash   string
}

// Digest is the SHA-256, in hex, of every field of r but Hash, in a fixed
// length-prefixed encoding, so the same record always has the same digest
// whatever encodes it. At is taken in UTC to the nanosecond.
func (r AuditRecord) Digest() string {
	h := sha256.New()
	field := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{':'})
		h.Write([]byte(s))
	}
	field("open-conduit.audit.v1")
	field(strconv.FormatUint(r.Sequence, 10))
	field(r.At.UTC().Format(time.RFC3339Nano))
	field(r.Actor)
	field(r.Via)
	field(r.Action)
	field(r.Target)
	field(r.Outcome)
	field(string(r.EventID))
	keys := make([]string, 0, len(r.Detail))
	for k := range r.Detail {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	field(strconv.Itoa(len(keys)))
	for _, k := range keys {
		field(k)
		field(r.Detail[k])
	}
	field(r.Prev)
	return hex.EncodeToString(h.Sum(nil))
}

// ErrAuditChain is a ledger that does not chain: a gap, a record out of
// order, or one whose Prev or Hash does not match.
var ErrAuditChain = errors.New("audit ledger does not chain")

// VerifyAuditChain checks that records continue the ledger from the record
// with sequence after and hash prev (0 and "" from the start): each one's
// Sequence is the next, its Prev the Hash before it, and its Hash its
// Digest. It returns the last sequence and hash checked, to continue from.
func VerifyAuditChain(after uint64, prev string, records []AuditRecord) (uint64, string, error) {
	for _, r := range records {
		switch {
		case r.Sequence != after+1:
			return after, prev, fmt.Errorf("%w: record %d follows %d", ErrAuditChain, r.Sequence, after)
		case r.Prev != prev:
			return after, prev, fmt.Errorf("%w: record %d does not follow the hash of record %d", ErrAuditChain, r.Sequence, after)
		case r.Hash != r.Digest():
			return after, prev, fmt.Errorf("%w: record %d does not match its hash", ErrAuditChain, r.Sequence)
		}
		after, prev = r.Sequence, r.Hash
	}
	return after, prev, nil
}

// SealAudit is r as storage appends it after the record numbered last
// whose hash is prev, at now: numbered, timed, chained and hashed, with a
// copy of its Detail. It is ok false when r has no Action or no Outcome.
func SealAudit(r AuditRecord, last uint64, prev string, now time.Time) (AuditRecord, bool) {
	if r.Action == "" || r.Outcome == "" {
		return AuditRecord{}, false
	}
	r.Sequence, r.At, r.Prev = last+1, now.UTC(), prev
	if len(r.Detail) > 0 {
		detail := make(map[string]string, len(r.Detail))
		for k, v := range r.Detail {
			detail[k] = v
		}
		r.Detail = detail
	} else {
		r.Detail = nil
	}
	r.Hash = r.Digest()
	return r, true
}
