package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

const causalNamespace = "core.causal"
const causalRejections = "core.causal.rejections"

// ErrCausalRejected identifies a refused branch, whose diagnostic can be
// committed without accepting the event or any associated child/timer effects.
var ErrCausalRejected = errors.New("causal activity rejected")

type CausalLimits struct{ MaxDepth, PerMinute int }

func normalizeCausalLimits(l CausalLimits) (CausalLimits, error) {
	if l.MaxDepth == 0 {
		l.MaxDepth = 32
	}
	if l.PerMinute == 0 {
		l.PerMinute = 128
	}
	if l.MaxDepth < 1 || l.PerMinute < 1 || l.MaxDepth > math.MaxInt32 || l.PerMinute > math.MaxInt32 {
		return l, storage.ErrInvalid
	}
	return l, nil
}

// CausalInfo is retained independently of payload retention. Root limits are
// pinned on the first causal operation and descendants cannot raise them.
type CausalInfo struct {
	Root   domain.ID
	Depth  int
	Limits CausalLimits
}
type CausalRejection struct {
	EventID, ParentID, RootID, WorkID, ExecutionID domain.ID
	DeliveryHash, Reason, TraceID                  string
	Depth, Count                                   int
	Limits                                         CausalLimits
	Minute                                         int64
	At                                             time.Time
}
type causalError struct{ diagnostic CausalRejection }

func (e *causalError) Error() string {
	return fmt.Sprintf("%s: %s", ErrCausalRejected, e.diagnostic.Reason)
}
func (e *causalError) Unwrap() error { return ErrCausalRejected }

type Causal struct {
	store  storage.Store
	now    func() time.Time
	limits CausalLimits
}

func NewCausal(store storage.Store, now func() time.Time, limits CausalLimits) (*Causal, error) {
	if store == nil || now == nil {
		return nil, storage.ErrInvalid
	}
	limits, err := normalizeCausalLimits(limits)
	if err != nil {
		return nil, err
	}
	return &Causal{store: store, now: now, limits: limits}, nil
}
func minuteOf(now time.Time) int64 {
	s := now.Unix()
	m := s / 60
	if s < 0 && s%60 != 0 {
		m--
	}
	return m
}
func causalKey(id domain.ID) string { return "event/" + identity(string(id)) }
func causalBucket(root domain.ID, minute int64) string {
	return fmt.Sprintf("bucket/%s/%d", identity(string(root)), minute)
}

func ancestry(tx storage.Tx, parent domain.ID, limits CausalLimits) (CausalInfo, error) {
	if parent == "" {
		return CausalInfo{}, storage.ErrInvalid
	}
	var trail []domain.ID
	seen := map[domain.ID]bool{}
	current := parent
	var info CausalInfo
	for {
		if seen[current] {
			return info, storage.ErrInvalid
		}
		seen[current] = true
		a, err := tx.Event(current)
		if err != nil {
			return info, err
		}
		err = getJSON(tx, causalNamespace, causalKey(current), &info)
		if err == nil {
			break
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return info, err
		}
		if a.Event.CausationID == "" {
			info = CausalInfo{Root: current, Limits: limits}
			break
		}
		trail = append(trail, current)
		current = a.Event.CausationID
	}
	// Existing envelopes accepted through trusted storage may predate this ledger.
	// Reconstruct their ancestry; only new acceptance charges the current bucket.
	info.Depth += len(trail)
	return info, nil
}
func deliveryDuplicate(tx storage.Tx, e domain.Event) (domain.AcceptedEvent, bool, error) {
	a, err := tx.EventByDelivery(e.Source, e.DeliveryID)
	if errors.Is(err, storage.ErrNotFound) {
		return domain.AcceptedEvent{}, false, nil
	}
	return a, err == nil, err
}
func checkDerived(tx storage.Tx, now time.Time, limits CausalLimits, parent domain.ID, e domain.Event) (CausalInfo, int, bool, error) {
	info, charges, duplicate, err := checkCausal(tx, now, limits, parent, "", e)
	count := 0
	if len(charges) > 0 {
		count = charges[0].count
	}
	return info, count, duplicate, err
}

// causalCharge is one chain's per-minute bucket a derived Event spends.
type causalCharge struct {
	root  domain.ID
	count int
}

// checkCausal checks a derived Event against its parent's chain. A floor
// (05-03 cited causation: the attempt's trigger when an extension cites
// another Event as the parent) also bounds it: the Event's depth is one more
// than the deeper of parent and floor, checked against the stricter of both
// chains' depth limits, and both chains' per-minute budgets must have room
// and are charged. A cited parent therefore never shortens a chain nor
// escapes the trigger's limits.
func checkCausal(tx storage.Tx, now time.Time, limits CausalLimits, parent, floor domain.ID, e domain.Event) (CausalInfo, []causalCharge, bool, error) {
	limits, err := normalizeCausalLimits(limits)
	if err != nil {
		return CausalInfo{}, nil, false, err
	}
	d := CausalRejection{EventID: e.ID, ParentID: parent, WorkID: e.WorkID, ExecutionID: e.ExecutionID, DeliveryHash: identity(e.Source, e.DeliveryID, string(parent)), Limits: limits, Minute: minuteOf(now), At: now}
	reject := func(reason string) (CausalInfo, []causalCharge, bool, error) {
		d.Reason = reason
		return CausalInfo{}, nil, false, &causalError{d}
	}
	if parent == "" || e.CausationID != "" && e.CausationID != parent {
		return reject("invalid_parent")
	}
	old, duplicate, err := deliveryDuplicate(tx, e)
	if err != nil {
		return CausalInfo{}, nil, false, err
	}
	if duplicate {
		if old.Event.CausationID != parent {
			return reject("delivery_parent_conflict")
		}
		return CausalInfo{}, nil, true, nil
	}
	info, err := ancestry(tx, parent, limits)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrInvalid) {
			return reject("missing_or_invalid_parent")
		}
		return info, nil, false, err
	}
	info.Depth++
	maxDepth := info.Limits.MaxDepth
	chains := []CausalInfo{info}
	if floor != "" && floor != parent {
		fl, err := ancestry(tx, floor, limits)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrInvalid) {
				return reject("missing_or_invalid_parent")
			}
			return info, nil, false, err
		}
		if fl.Depth+1 > info.Depth {
			info.Depth = fl.Depth + 1
		}
		maxDepth = min(maxDepth, fl.Limits.MaxDepth)
		if fl.Root != info.Root {
			chains = append(chains, fl)
		}
	}
	d.RootID = info.Root
	d.Depth = info.Depth
	d.Limits = info.Limits
	charges := make([]causalCharge, 0, len(chains))
	for _, c := range chains {
		count := 0
		err = getJSON(tx, causalNamespace, causalBucket(c.Root, d.Minute), &count)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return info, nil, false, err
		}
		charges = append(charges, causalCharge{root: c.Root, count: count})
	}
	d.Count = charges[0].count
	if e.ExecutionID != "" {
		x, er := tx.Execution(e.ExecutionID)
		if er == nil {
			d.TraceID = x.TraceID
		} else if !errors.Is(er, storage.ErrNotFound) {
			return info, nil, false, er
		}
	}
	if info.Depth > maxDepth {
		return reject("depth_limit")
	}
	for i, c := range charges {
		if c.count >= chains[i].Limits.PerMinute {
			d.RootID, d.Count, d.Limits = c.root, c.count, chains[i].Limits
			return reject("rate_limit")
		}
	}
	return info, charges, false, nil
}
func recordCausalRejection(tx storage.Tx, err error) error {
	var refused *causalError
	if !errors.As(err, &refused) {
		return storage.ErrInvalid
	}
	d := refused.diagnostic
	return putJSON(tx, causalRejections, identity(d.DeliveryHash, string(d.ParentID)), d)
}

// CheckDerivedInTx preflights ancestry and budgets before associated effects.
// On ErrCausalRejected it writes only a redacted diagnostic. The caller must
// commit that refusal and apply no associated effects. Other errors roll back.
// A success reserves nothing; call AcceptDerivedInTx in the SAME transaction.
func CheckDerivedInTx(tx storage.Tx, now time.Time, limits CausalLimits, parent domain.ID, e domain.Event) error {
	_, _, _, err := checkDerived(tx, now, limits, parent, e)
	if errors.Is(err, ErrCausalRejected) {
		if auditErr := recordCausalRejection(tx, err); auditErr != nil {
			return auditErr
		}
	}
	return err
}

// AcceptDerivedInTx is trusted core authority: parent is selected from a
// committed route trigger or fenced execution, never from caller input. On a
// refusal the caller must commit only the diagnostic, or roll back all effects
// and record the refusal separately. Acknowledge acceptance only after commit.
func AcceptDerivedInTx(tx storage.Tx, now time.Time, limits CausalLimits, scope, parent domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error) {
	return acceptCausalInTx(tx, now, limits, scope, parent, "", e)
}

// acceptCausalInTx is AcceptDerivedInTx with an optional floor (checkCausal).
func acceptCausalInTx(tx storage.Tx, now time.Time, limits CausalLimits, scope, parent, floor domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error) {
	info, charges, duplicate, err := checkCausal(tx, now, limits, parent, floor, e)
	if err != nil {
		if errors.Is(err, ErrCausalRejected) {
			if auditErr := recordCausalRejection(tx, err); auditErr != nil {
				return domain.AcceptedEvent{}, false, auditErr
			}
		}
		return domain.AcceptedEvent{}, false, err
	}
	e.CausationID = parent
	accepted, added, err := AcceptInTx(tx, scope, e)
	if err != nil {
		return domain.AcceptedEvent{}, false, err
	}
	if duplicate || !added {
		return accepted, false, nil
	}
	// Pin root's policy separately so future siblings survive config changes.
	rootInfo := CausalInfo{Root: info.Root, Limits: info.Limits}
	if err = putJSON(tx, causalNamespace, causalKey(info.Root), rootInfo); err != nil {
		return domain.AcceptedEvent{}, false, err
	}
	if err = putJSON(tx, causalNamespace, causalKey(accepted.Event.ID), info); err != nil {
		return domain.AcceptedEvent{}, false, err
	}
	for _, c := range charges {
		if err = putJSON(tx, causalNamespace, causalBucket(c.root, minuteOf(now)), c.count+1); err != nil {
			return domain.AcceptedEvent{}, false, err
		}
	}
	return accepted, true, nil
}

// Derive is for trusted routes/subscription translators and builtins. Executor
// extensions must use Emit, which selects ancestry from the fenced attempt.
func (c *Causal) Derive(ctx context.Context, scope, parent domain.ID, e domain.Event) (accepted domain.AcceptedEvent, added bool, err error) {
	var refusal error
	now := c.now()
	err = c.store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		var er error
		accepted, added, er = AcceptDerivedInTx(tx, now, c.limits, scope, parent, e)
		if errors.Is(er, ErrCausalRejected) {
			refusal = er
			return nil
		}
		return er
	})
	if err != nil {
		return domain.AcceptedEvent{}, false, err
	}
	if refusal != nil {
		return domain.AcceptedEvent{}, false, refusal
	}
	return
}

// Emit derives an extension event from this attempt's immutable trigger. The
// storage guard validates a live token before any event, counter or KV mutation.
// Identity, core families and cross-Work targeting cannot be supplied by it.
func (c *Causal) Emit(ctx context.Context, lease domain.LeaseRef, e domain.Event) (accepted domain.AcceptedEvent, added bool, err error) {
	now := c.now()
	var refusal error
	err = c.store.Transact(ctx, storage.Guard{Now: now, Lease: &lease}, func(tx storage.Tx) error {
		x, er := tx.Execution(lease.ExecutionID)
		if er != nil {
			return er
		}
		w, er := tx.Work(x.WorkID)
		if er != nil {
			return er
		}
		if strings.HasPrefix(e.Type, "core.") || strings.HasPrefix(e.Source, "core.") || e.WorkID != "" && e.WorkID != w.ID || e.ExecutionID != "" && e.ExecutionID != x.ID {
			return storage.ErrInvalid
		}
		e.WorkID = w.ID
		e.ExecutionID = x.ID
		trigger, er := executionTriggerInTx(tx, x)
		if er != nil {
			return er
		}
		accepted, added, er = AcceptDerivedInTx(tx, now, c.limits, w.ScopeID, trigger, e)
		if errors.Is(er, ErrCausalRejected) {
			refusal = er
			return nil
		}
		return er
	})
	if err != nil {
		return domain.AcceptedEvent{}, false, err
	}
	if refusal != nil {
		return domain.AcceptedEvent{}, false, refusal
	}
	return
}
func (c *Causal) RecordRejection(ctx context.Context, err error) error {
	return c.store.Transact(ctx, storage.Guard{Now: c.now()}, func(tx storage.Tx) error { return recordCausalRejection(tx, err) })
}
func (c *Causal) Rejections(ctx context.Context) (out []CausalRejection, err error) {
	out = []CausalRejection{}
	err = c.store.Transact(ctx, storage.Guard{Now: c.now()}, func(tx storage.Tx) error {
		rows, er := tx.List(causalRejections, "")
		if er != nil {
			return er
		}
		for _, row := range rows {
			var d CausalRejection
			if er = getJSON(tx, causalRejections, row.Key, &d); er != nil {
				return er
			}
			out = append(out, d)
		}
		return nil
	})
	return
}
