package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/storage"
)

const (
	tokenNamespace   = "core.toolbelt.tokens"
	tokenIDNamespace = "core.toolbelt.token_ids"
	requestNamespace = "core.toolbelt.requests"
)

// ErrRequestConflict reports a reused toolbelt request ID with a different body.
var ErrRequestConflict = errors.New("toolbelt request identity reused with different input")

// AuthorityCheck is the current-ceiling question asked before every call. The
// binding and generation are pinned by the attempt; config answers whether the
// operation is still enabled and within the scope ceiling. It can only narrow.
type AuthorityCheck struct {
	ScopeID, WorkID, ExecutionID domain.ID
	Binding, Generation          string
	Operation                    string
}

// ToolbeltGrant is the pinned operation/event allowlist for one new token,
// and the connection the attempt's binding names ("" for none), which the
// token's authority carries to operation handlers.
type ToolbeltGrant struct {
	Operations, EventTypes []string
	Connection             string
}

type ToolbeltOptions struct {
	CausalLimits CausalLimits
	// Grant selects the pinned allowlist from trusted config when the scheduler
	// issues a token. A nil Grant issues tokens with no operations.
	Grant func(context.Context, AttemptState) (ToolbeltGrant, error)
	// Authorize enforces current scope/ancestor ceiling and enabled state. It is
	// called outside storage transactions; a nil Authorize permits the pinned grant.
	Authorize func(context.Context, AuthorityCheck) error
	// Lifetime bounds a token after issue; zero uses the attempt deadline or one hour.
	Lifetime time.Duration
	// AuthorizeAlias answers whether a translated wait may depend on another
	// scope's correlations (B04 cross-scope aliases). It is called outside
	// storage transactions when the wait is declared; registration rechecks it.
	// A nil AuthorizeAlias refuses every cross-scope dependency.
	AuthorizeAlias func(context.Context, AliasAuthorization) error
	// Observer receives a toolbelt.called or toolbelt.rejected record for every
	// call whose credential resolves to an execution authority. State changes
	// made by a call are reported separately by the instrumented store.
	Observer observability.Observer
}

// Toolbelt is the production B03 authority path. Token plaintext is returned
// once to the trusted issuer and only its SHA-256 digest is persisted.
type Toolbelt struct {
	store   storage.Store
	now     func() time.Time
	router  *Router
	options ToolbeltOptions

	mu            sync.RWMutex
	registrations map[string]bool
	operations    map[string]extension.Operation
	owners        map[string]string // operation → registration name
	filters       map[string]func(domain.Payload, domain.Event) bool
	events        map[string]string // emitted family → registration name
}

type tokenRecord struct {
	TokenID             string
	Authority           extension.Authority
	Binding, Generation string
	Revoked             bool
	RevokedAt           time.Time
}

// ToolbeltDeclaration is a toolbelt wait/outcome request recorded under the
// current fence. It releases toolbelt authority immediately, but is applied only
// when the trusted host observes the executor finishing or suspending this
// segment through Scheduler.Complete; a request alone is never exclusion proof.
type ToolbeltDeclaration struct {
	Epoch   uint64
	Outcome *domain.Outcome
	Wait    *domain.Wait
	Timer   *domain.Timer
	// Subscriptions are a translated wait's dependency aliases, installed
	// once the Wait is registered (see Scheduler.ReconcileSubscriptions).
	Subscriptions []SubscriptionRequest `json:",omitempty"`
}

type toolbeltReplay struct {
	Hash   string
	Result extension.ToolResult
}

func NewToolbelt(store storage.Store, now func() time.Time, router *Router, options ToolbeltOptions) (*Toolbelt, error) {
	if store == nil || now == nil || router == nil || options.Lifetime < 0 {
		return nil, storage.ErrInvalid
	}
	limits, err := normalizeCausalLimits(options.CausalLimits)
	if err != nil {
		return nil, err
	}
	options.CausalLimits = limits
	return &Toolbelt{store: store, now: now, router: router, options: options, registrations: map[string]bool{}, operations: map[string]extension.Operation{}, owners: map[string]string{}, filters: map[string]func(domain.Payload, domain.Event) bool{}, events: map[string]string{}}, nil
}

var operationName = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)

func builtinOperation(name string) bool {
	switch name {
	case extension.Emit, extension.CreateWork, extension.Wait, extension.DeclareOutcome, extension.ReadContext:
		return true
	}
	return false
}

// Register atomically declares an extension's operations and emitted families.
// Registration grants nothing; tokens must still list each operation.
func (t *Toolbelt) Register(r extension.Registration) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.Version != extension.ContractVersion || r.Name == "" || strings.ContainsAny(r.Name, " \t\r\n") {
		return extension.ErrInvalid
	}
	if t.registrations[r.Name] {
		return extension.ErrConflict
	}
	names, events := map[string]bool{}, map[string]bool{}
	for _, op := range r.Operations {
		set := 0
		for _, ok := range []bool{op.Handler != nil, op.Wait != nil, op.Emit != nil} {
			if ok {
				set++
			}
		}
		if set != 1 || !operationName.MatchString(op.Name) || len(op.Guidance) > extension.MaxGuidance {
			return extension.ErrInvalid
		}
		if builtinOperation(op.Name) || t.declaredLocked(op.Name) || names[op.Name] {
			return extension.ErrConflict
		}
		names[op.Name] = true
	}
	for _, name := range r.EventTypes {
		if !validType(name) || strings.HasPrefix(name, "core.") {
			return extension.ErrInvalid
		}
		if events[name] || t.events[name] != "" {
			return extension.ErrConflict
		}
		events[name] = true
	}
	t.registrations[r.Name] = true
	for _, op := range r.Operations {
		t.operations[op.Name] = op
		t.owners[op.Name] = r.Name
	}
	if r.DependencyFilter != nil {
		t.filters[r.Name] = r.DependencyFilter
	}
	for name := range events {
		t.events[name] = r.Name
	}
	return nil
}

// Operations lists builtins and registered extension operations.
func (t *Toolbelt) Operations() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := []string{extension.CreateWork, extension.DeclareOutcome, extension.Emit, extension.ReadContext, extension.Wait}
	for name := range t.operations {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Guidance is the agent guidance of the operations the token's pinned grant
// includes and current config still enables, in grant order, without
// repeats. It reveals nothing beyond what the grant already names. A token
// that is revoked, expired or issued to another execution than the asserted
// one is unauthorized.
func (t *Toolbelt) Guidance(ctx context.Context, token string, execution domain.ID) ([]string, error) {
	record, err := t.load(ctx, token)
	if err != nil {
		return nil, err
	}
	a := record.Authority
	if record.Revoked || !t.now().Before(a.ExpiresAt) || execution != "" && execution != a.ExecutionID {
		return nil, extension.ErrUnauthorized
	}
	var out []string
	seen := map[string]bool{}
	for _, name := range a.Operations {
		t.mu.RLock()
		text := t.operations[name].Guidance
		t.mu.RUnlock()
		if text == "" || seen[text] {
			continue
		}
		if t.options.Authorize != nil && t.options.Authorize(ctx, AuthorityCheck{ScopeID: a.ScopeID, WorkID: a.WorkID, ExecutionID: a.ExecutionID, Binding: record.Binding, Generation: record.Generation, Operation: name}) != nil {
			continue
		}
		seen[text] = true
		out = append(out, text)
	}
	return out, nil
}

// EventTypes lists registered emitted event families.
func (t *Toolbelt) EventTypes() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.events))
	for name := range t.events {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (t *Toolbelt) declared(op string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return builtinOperation(op) || t.declaredLocked(op)
}
func (t *Toolbelt) declaredLocked(op string) bool {
	o := t.operations[op]
	return o.Handler != nil || o.Wait != nil || o.Emit != nil
}

// DependencyFilter evaluates a translated dependency's opaque filter with the
// filter its registering extension declared. Composition passes it to the
// scheduler (SchedulerOptions.AliasFilter); an unknown namespace never matches.
func (t *Toolbelt) DependencyFilter(namespace string, filter domain.Payload, e domain.Event) bool {
	t.mu.RLock()
	f := t.filters[namespace]
	t.mu.RUnlock()
	return f != nil && f(filter, e)
}
func (t *Toolbelt) declaredEvent(name string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.events[name] != ""
}

// TokenDigest is the persisted lookup key for a bearer credential.
func TokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewBearer returns an unpadded base64url encoding of 32 random bytes.
func NewBearer() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// issueInTx validates the current lease and stores a fresh digest. Older-epoch
// tokens for the same execution are revoked in the same transaction.
func (t *Toolbelt) issueInTx(tx storage.Tx, a extension.Authority) (string, string, error) {
	now := t.now()
	if a.ScopeID == "" || a.WorkID == "" || a.ExecutionID == "" || a.Lease.ExecutionID != a.ExecutionID || !now.Before(a.ExpiresAt) {
		return "", "", extension.ErrUnauthorized
	}
	for _, op := range a.Operations {
		if !t.declared(op) {
			return "", "", extension.ErrUndeclared
		}
	}
	for _, name := range a.EventTypes {
		if !t.declaredEvent(name) {
			return "", "", extension.ErrUndeclared
		}
	}
	x, err := tx.Execution(a.ExecutionID)
	if err != nil {
		return "", "", err
	}
	w, err := tx.Work(x.WorkID)
	if err != nil {
		return "", "", err
	}
	state, err := attemptState(tx, x.ID)
	if err != nil {
		return "", "", err
	}
	if x.State != domain.ExecutionRunning || x.WorkID != a.WorkID || w.ScopeID != a.ScopeID || x.Lease.Epoch != a.Lease.Epoch || x.Lease.Owner != a.Lease.Owner || state.Epoch != a.Lease.Epoch {
		return "", "", extension.ErrUnauthorized
	}
	if !state.Deadline.IsZero() && state.Deadline.Before(a.ExpiresAt) {
		a.ExpiresAt = state.Deadline
	}
	if a.TriggerID == "" {
		if a.TriggerID, err = executionTriggerInTx(tx, x); err != nil {
			return "", "", err
		}
	}
	if err = t.revokeExecutionInTx(tx, x.ID, a.Lease.Epoch); err != nil {
		return "", "", err
	}
	token, err := NewBearer()
	if err != nil {
		return "", "", err
	}
	id, err := NewBearer()
	if err != nil {
		return "", "", err
	}
	id = "xtok_" + id[:22]
	a.Operations = append([]string(nil), a.Operations...)
	a.EventTypes = append([]string(nil), a.EventTypes...)
	record := tokenRecord{TokenID: id, Authority: a, Binding: state.Binding.Name, Generation: state.Generation}
	digest := TokenDigest(token)
	if err = putJSON(tx, tokenNamespace, digest, record); err != nil {
		return "", "", err
	}
	if err = putJSON(tx, tokenIDNamespace, id, digest); err != nil {
		return "", "", err
	}
	if err = putJSON(tx, tokenNamespace+".execution", identity(string(x.ID))+"/"+id, digest); err != nil {
		return "", "", err
	}
	return token, id, nil
}

func (t *Toolbelt) revokeExecutionInTx(tx storage.Tx, execution domain.ID, below uint64) error {
	rows, err := tx.List(tokenNamespace+".execution", identity(string(execution))+"/")
	if err != nil {
		return err
	}
	for _, row := range rows {
		var digest string
		if err = json.Unmarshal(row.Value, &digest); err != nil {
			return err
		}
		var record tokenRecord
		if err = getJSON(tx, tokenNamespace, digest, &record); err != nil {
			return err
		}
		if record.Revoked || record.Authority.Lease.Epoch >= below {
			continue
		}
		record.Revoked, record.RevokedAt = true, t.now()
		if err = putJSON(tx, tokenNamespace, digest, record); err != nil {
			return err
		}
	}
	return nil
}

// Issue is trusted runtime authority. It requires the named lease to be the
// execution's current running lease and returns plaintext exactly once.
func (t *Toolbelt) Issue(ctx context.Context, a extension.Authority) (token string, err error) {
	_, token, err = t.IssueWithID(ctx, a)
	return
}

// IssueWithID returns the revocation ID alongside the token.
func (t *Toolbelt) IssueWithID(ctx context.Context, a extension.Authority) (id, token string, err error) {
	err = t.store.Transact(ctx, storage.Guard{Now: t.now(), Lease: &a.Lease}, func(tx storage.Tx) error {
		var e error
		token, id, e = t.issueInTx(tx, a)
		return e
	})
	if errors.Is(err, storage.ErrLeaseLost) {
		err = extension.ErrUnauthorized
	}
	return
}

// issueForAttempt is called by the scheduler after claim/recovery/resume and
// before the executor sees its request.
func (t *Toolbelt) issueForAttempt(ctx context.Context, ref domain.LeaseRef, state AttemptState) (string, error) {
	grant := ToolbeltGrant{}
	if t.options.Grant != nil {
		var err error
		if grant, err = t.options.Grant(ctx, state); err != nil {
			return "", err
		}
	}
	expires := state.Deadline
	if t.options.Lifetime > 0 || expires.IsZero() {
		lifetime := t.options.Lifetime
		if lifetime == 0 {
			lifetime = time.Hour
		}
		if candidate := t.now().Add(lifetime); expires.IsZero() || candidate.Before(expires) {
			expires = candidate
		}
	}
	return t.Issue(ctx, extension.Authority{ScopeID: state.ScopeID, WorkID: state.WorkID, ExecutionID: state.ExecutionID, Lease: ref, ExpiresAt: expires, Operations: grant.Operations, EventTypes: grant.EventTypes, Connection: grant.Connection})
}

// Revoke is idempotent; unknown tokens are already unusable.
func (t *Toolbelt) Revoke(ctx context.Context, token string) error {
	return t.revokeDigest(ctx, TokenDigest(token))
}

// RevokeID revokes a known execution token by its opaque ID. It reports
// storage.ErrNotFound for unknown IDs and returns the revocation time.
func (t *Toolbelt) RevokeID(ctx context.Context, id string) (revokedAt time.Time, err error) {
	err = t.store.Transact(ctx, storage.Guard{Now: t.now()}, func(tx storage.Tx) error {
		var digest string
		if e := getJSON(tx, tokenIDNamespace, id, &digest); e != nil {
			return e
		}
		var e error
		revokedAt, e = t.revokeInTx(tx, digest)
		return e
	})
	return
}
func (t *Toolbelt) revokeDigest(ctx context.Context, digest string) error {
	return t.store.Transact(ctx, storage.Guard{Now: t.now()}, func(tx storage.Tx) error {
		_, err := t.revokeInTx(tx, digest)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	})
}
func (t *Toolbelt) revokeInTx(tx storage.Tx, digest string) (time.Time, error) {
	var record tokenRecord
	if err := getJSON(tx, tokenNamespace, digest, &record); err != nil {
		return time.Time{}, err
	}
	if record.Revoked {
		return record.RevokedAt, nil
	}
	record.Revoked, record.RevokedAt = true, t.now()
	return record.RevokedAt, putJSON(tx, tokenNamespace, digest, record)
}

// Known reports whether a bearer belongs to the execution credential class,
// without validating it. Management uses it only for class separation.
func (t *Toolbelt) Known(ctx context.Context, token string) (known bool, err error) {
	err = t.store.Transact(ctx, storage.Guard{Now: t.now()}, func(tx storage.Tx) error {
		_, e := tx.Get(tokenNamespace, TokenDigest(token))
		if errors.Is(e, storage.ErrNotFound) {
			return nil
		}
		known = e == nil
		return e
	})
	return
}

func (t *Toolbelt) load(ctx context.Context, token string) (record tokenRecord, err error) {
	err = t.store.Transact(ctx, storage.Guard{Now: t.now()}, func(tx storage.Tx) error {
		return getJSON(tx, tokenNamespace, TokenDigest(token), &record)
	})
	if errors.Is(err, storage.ErrNotFound) {
		err = extension.ErrUnauthorized
	}
	return
}

// authorizedInTx revalidates the token, lease and execution segment under the
// lease guard. Every call, including reads, passes through it.
func (t *Toolbelt) authorizedInTx(tx storage.Tx, digest string) (tokenRecord, domain.Execution, domain.Work, AttemptState, error) {
	var record tokenRecord
	var x domain.Execution
	var w domain.Work
	var state AttemptState
	if err := getJSON(tx, tokenNamespace, digest, &record); err != nil {
		return record, x, w, state, extension.ErrUnauthorized
	}
	a := record.Authority
	if record.Revoked || !t.now().Before(a.ExpiresAt) {
		return record, x, w, state, extension.ErrUnauthorized
	}
	var err error
	if x, err = tx.Execution(a.ExecutionID); err != nil {
		return record, x, w, state, err
	}
	if w, err = tx.Work(x.WorkID); err != nil {
		return record, x, w, state, err
	}
	if state, err = attemptState(tx, x.ID); err != nil {
		return record, x, w, state, err
	}
	if x.State != domain.ExecutionRunning || x.Lease.Epoch != a.Lease.Epoch || x.Lease.Owner != a.Lease.Owner || !t.now().Before(x.Lease.ExpiresAt) || state.Epoch != a.Lease.Epoch || state.Declaration != nil || x.WorkID != a.WorkID || w.ScopeID != a.ScopeID || w.State != domain.WorkRunning {
		return record, x, w, state, extension.ErrUnauthorized
	}
	return record, x, w, state, nil
}

// notGranted refuses an operation the token does not carry, naming it and
// where to grant it, so the caller can tell the owner what to add. It is
// still ErrUnauthorized; only the refusal's words differ from a revoked or
// mismatched token's. The caller can't use the operation either way, so
// naming it leaks nothing.
func notGranted(operation, binding string, scope domain.ID) error {
	where := "this execution's binding"
	if binding != "" {
		where = "binding " + binding
	}
	return errors.Join(extension.ErrUnauthorized, &extension.OperationError{Code: "not_granted",
		Message: fmt.Sprintf("%s isn't granted to %s in scope %s; to allow it, add it to that binding's operations and to the scope's authorization.operations", operation, where, scope)})
}

// callCode classifies a call result without provider or request text.
func callCode(err error) string {
	var op *extension.OperationError
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &op) && op.Code == "not_granted":
		return "not_granted"
	case errors.Is(err, extension.ErrUnauthorized), errors.Is(err, storage.ErrLeaseLost), errors.Is(err, storage.ErrNotFound):
		return "unauthorized"
	case errors.Is(err, extension.ErrUndeclared):
		return "undeclared"
	case errors.Is(err, ErrCausalRejected):
		return "causal_limit"
	case errors.Is(err, ErrRequestConflict):
		return "request_conflict"
	case errors.Is(err, storage.ErrConflict):
		return "conflict"
	case errors.Is(err, storage.ErrInvalid), errors.Is(err, extension.ErrInvalid), errors.Is(err, extension.ErrUnsupported):
		return "invalid"
	}
	return "failed"
}

func (t *Toolbelt) observeCall(ctx context.Context, record tokenRecord, operation, requestID string, callErr error) {
	a := record.Authority
	name := "toolbelt.called"
	if callErr != nil {
		name = "toolbelt.rejected"
	}
	rec := observability.Record{Name: name, At: t.now(), Correlation: observability.Correlation{ScopeID: a.ScopeID, WorkID: a.WorkID, ExecutionID: a.ExecutionID, ExecutorBinding: record.Binding}, Attributes: map[string]string{"toolbelt.operation": operation, "toolbelt.result": callCode(callErr), "lease.epoch": strconv.FormatUint(a.Lease.Epoch, 10)}}
	// A call has no durable revision; a per-call nonce keeps the identity of
	// this one record stable across exporter retries without colliding.
	nonce, _ := randomToken()
	rec.Attributes["record_id"] = identity(name, string(a.ExecutionID), strconv.FormatUint(a.Lease.Epoch, 10), operation, requestID, nonce)
	if err := t.store.Transact(context.WithoutCancel(ctx), storage.Guard{Now: t.now()}, func(tx storage.Tx) error {
		x, e := tx.Execution(a.ExecutionID)
		if e != nil {
			return e
		}
		rec.Correlation.TraceID = x.TraceID
		trigger, e := executionTriggerInTx(tx, x)
		if e != nil {
			return e
		}
		rec.Correlation.EventID = trigger
		return nil
	}); err != nil {
		return
	}
	observeDirect(ctx, t.options.Observer, rec)
}

func requestHash(c extension.ToolCall) string {
	c.ScopeID, c.WorkID, c.ExecutionID = "", "", ""
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Call is the one execution-facing surface. requestID optionally gives the
// call an idempotency identity within the same execution; see CallRequest.
func (t *Toolbelt) Call(ctx context.Context, token string, c extension.ToolCall) (extension.ToolResult, error) {
	return t.CallRequest(ctx, token, "", c)
}

// CallRequest replays a committed result for the same execution, operation and
// request ID while authority is still valid. Reuse with a different body is
// ErrRequestConflict. Wait/outcome release authority, so duplicates after a lost
// response fail authorization and must be resolved by inspection.
func (t *Toolbelt) CallRequest(ctx context.Context, token, requestID string, c extension.ToolCall) (out extension.ToolResult, err error) {
	defer func() {
		if errors.Is(err, storage.ErrLeaseLost) || errors.Is(err, storage.ErrNotFound) {
			err = extension.ErrUnauthorized
		}
	}()
	if len(requestID) > 128 {
		return out, extension.ErrInvalid
	}
	record, err := t.load(ctx, token)
	if err != nil {
		return out, err
	}
	if t.options.Observer != nil {
		defer func() { t.observeCall(ctx, record, c.Operation, requestID, err) }()
	}
	a := record.Authority
	if record.Revoked || !t.now().Before(a.ExpiresAt) || c.ScopeID != "" && c.ScopeID != a.ScopeID || c.WorkID != "" && c.WorkID != a.WorkID || c.ExecutionID != "" && c.ExecutionID != a.ExecutionID {
		return out, extension.ErrUnauthorized
	}
	if !t.declared(c.Operation) {
		return out, extension.ErrUndeclared
	}
	if !contains(a.Operations, c.Operation) {
		return out, notGranted(c.Operation, record.Binding, a.ScopeID)
	}
	if t.options.Authorize != nil {
		if e := t.options.Authorize(ctx, AuthorityCheck{ScopeID: a.ScopeID, WorkID: a.WorkID, ExecutionID: a.ExecutionID, Binding: record.Binding, Generation: record.Generation, Operation: c.Operation}); e != nil {
			return out, extension.ErrUnauthorized
		}
	}
	digest := TokenDigest(token)
	hash := requestHash(c)
	replayKey := ""
	if requestID != "" {
		replayKey = identity(string(a.ExecutionID), c.Operation, requestID)
	}
	ref := a.Lease
	now := t.now()
	if !builtinOperation(c.Operation) {
		// Extension handlers run outside storage after a fenced authority check
		// and replay lookup; their committed result is recorded under the fence.
		var replay *toolbeltReplay
		err = t.store.Transact(ctx, storage.Guard{Now: now, Lease: &ref}, func(tx storage.Tx) error {
			if _, _, _, _, e := t.authorizedInTx(tx, digest); e != nil {
				return e
			}
			var e error
			replay, e = t.replayInTx(tx, replayKey, hash)
			return e
		})
		if err != nil || replay != nil {
			if replay != nil {
				out = replay.Result
			}
			return out, err
		}
		t.mu.RLock()
		op := t.operations[c.Operation]
		owner := t.owners[c.Operation]
		t.mu.RUnlock()
		handlerAuthority := a
		handlerAuthority.Operations = append([]string(nil), a.Operations...)
		handlerAuthority.EventTypes = append([]string(nil), a.EventTypes...)
		hctx := extension.WithCallRequestID(ctx, requestID)
		if op.Wait != nil {
			return t.declareTranslatedWait(hctx, record, digest, owner, op, handlerAuthority, c.Input)
		}
		if op.Emit != nil {
			return t.translatedEmit(hctx, digest, owner, op, handlerAuthority, c.Input, replayKey, hash)
		}
		output, e := op.Handler(hctx, handlerAuthority, c.Input)
		if e != nil {
			return out, e
		}
		out.Output = output
		if replayKey == "" {
			return out, nil
		}
		err = t.store.Transact(ctx, storage.Guard{Now: t.now(), Lease: &ref}, func(tx storage.Tx) error {
			if _, _, _, _, e := t.authorizedInTx(tx, digest); e != nil {
				return e
			}
			return putJSON(tx, requestNamespace, replayKey, toolbeltReplay{Hash: hash, Result: out})
		})
		return out, err
	}
	var refusal error
	err = t.store.Transact(ctx, storage.Guard{Now: now, Lease: &ref}, func(tx storage.Tx) error {
		_, x, w, state, e := t.authorizedInTx(tx, digest)
		if e != nil {
			return e
		}
		replay, e := t.replayInTx(tx, replayKey, hash)
		if e != nil || replay != nil {
			if replay != nil {
				out = replay.Result
			}
			return e
		}
		trigger, e := executionTriggerInTx(tx, x)
		if e != nil {
			return e
		}
		switch c.Operation {
		case extension.ReadContext:
			out.Context = w.Context
		case extension.Emit:
			if c.Event == nil {
				return extension.ErrInvalid
			}
			ev := *c.Event
			if !t.declaredEvent(ev.Type) || !contains(a.EventTypes, ev.Type) {
				return extension.ErrUndeclared
			}
			if ev.CausationID != "" && ev.CausationID != trigger || ev.WorkID != "" && ev.WorkID != w.ID || ev.ExecutionID != "" && ev.ExecutionID != x.ID || ev.Source != "" && ev.Source != "execution/"+string(x.ID) {
				return extension.ErrUnauthorized
			}
			if ev.DeliveryID == "" || strings.HasPrefix(ev.Type, "core.") {
				return extension.ErrInvalid
			}
			if ev.CorrelationKey != nil && !containsKey(w, *ev.CorrelationKey) {
				return extension.ErrUnauthorized
			}
			ev.WorkID, ev.ExecutionID, ev.Source = w.ID, x.ID, "execution/"+string(x.ID)
			ev.ID = domain.ID("emit/" + identity(string(x.ID), ev.DeliveryID))
			ev.ReceivedAt = now
			if ev.OccurredAt.IsZero() {
				ev.OccurredAt = now
			}
			accepted, _, er := AcceptDerivedInTx(tx, now, t.options.CausalLimits, w.ScopeID, trigger, ev)
			if errors.Is(er, ErrCausalRejected) {
				refusal = er
				return nil
			}
			if er != nil {
				return er
			}
			out.Event = &accepted
		case extension.CreateWork:
			if c.Child == nil {
				return extension.ErrInvalid
			}
			child := *c.Child
			if child.ScopeID != "" && child.ScopeID != w.ScopeID || child.ParentID != "" && child.ParentID != w.ID || child.State != "" && child.State != domain.WorkReady || child.Revision != 0 {
				return extension.ErrUnauthorized
			}
			if child.ID == "" {
				seed := requestID
				if seed == "" {
					if seed, e = NewBearer(); e != nil {
						return e
					}
				}
				child.ID = domain.ID("child/" + identity(string(x.ID), seed))
			}
			var rejected error
			saved, er := t.router.createLinked(tx, w.ScopeID, w.ID, "", trigger, ChildRequest{ID: child.ID, Kind: child.Kind, Keys: child.CorrelationKeys, Context: child.Context}, &rejected)
			if er != nil {
				return er
			}
			if rejected != nil {
				refusal = rejected
				return nil
			}
			created, er := tx.Event(domain.ID("work-created/" + identity(string(child.ID))))
			if er != nil {
				return er
			}
			out.Work, out.Event = &saved, &created
		case extension.DeclareOutcome:
			if c.Outcome == nil {
				return extension.ErrInvalid
			}
			if e = extension.ValidateResult(extension.Result{Outcome: *c.Outcome}); e != nil {
				return e
			}
			if c.Outcome.Kind == domain.OutcomeWaiting {
				return extension.ErrInvalid
			}
			outcome := *c.Outcome
			state.Declaration = &ToolbeltDeclaration{Epoch: ref.Epoch, Outcome: &outcome}
			if e = putAttemptState(tx, state); e != nil {
				return e
			}
		case extension.Wait:
			if c.Wait == nil {
				return extension.ErrInvalid
			}
			decl, er := t.declareWaitInTx(tx, x, w, state, trigger, *c.Wait, now)
			if er != nil {
				return er
			}
			state.Declaration = decl
			if e = putAttemptState(tx, state); e != nil {
				return e
			}
			planned := *decl.Wait
			out.Wait = &planned
		}
		if replayKey != "" && c.Operation != extension.Wait && c.Operation != extension.DeclareOutcome {
			return putJSON(tx, requestNamespace, replayKey, toolbeltReplay{Hash: hash, Result: out})
		}
		return nil
	})
	if err == nil && refusal != nil {
		// The refusal diagnostic committed without any associated effects.
		return extension.ToolResult{}, refusal
	}
	return out, err
}

func (t *Toolbelt) replayInTx(tx storage.Tx, key, hash string) (*toolbeltReplay, error) {
	if key == "" {
		return nil, nil
	}
	var replay toolbeltReplay
	err := getJSON(tx, requestNamespace, key, &replay)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if replay.Hash != hash {
		return nil, ErrRequestConflict
	}
	return &replay, nil
}

// declareWaitInTx validates the predicate under the fence and allocates the
// wait/timer identities. The exclusive watermark defaults to the attempt's
// acknowledged input, so final registration backscans everything after it.
func (t *Toolbelt) declareWaitInTx(tx storage.Tx, x domain.Execution, w domain.Work, state AttemptState, trigger domain.ID, request extension.WaitRequest, now time.Time) (*ToolbeltDeclaration, error) {
	if len(request.Match.Keys) == 0 && len(request.Match.Types) == 0 {
		return nil, extension.ErrInvalid
	}
	// A Wait is a subscription on its Work's keys (B04). A type-only request
	// subscribes on every key the Work owns; Work without keys cannot wait, and
	// is refused here rather than at the later registration handshake.
	if len(request.Match.Keys) == 0 {
		if len(w.CorrelationKeys) == 0 {
			return nil, extension.ErrInvalid
		}
		request.Match.Keys = append([]domain.CorrelationKey(nil), w.CorrelationKeys...)
	}
	for _, key := range request.Match.Keys {
		if !containsKey(w, key) {
			return nil, extension.ErrUnauthorized
		}
	}
	for _, name := range request.Match.Types {
		if !validType(name) {
			return nil, extension.ErrInvalid
		}
	}
	// A description is for operator views: an agent's or an extension's
	// that does not fit is cut to one line within the limit, never refused.
	request.Description = extension.Clip(request.Description, extension.MaxWaitDescription)
	if request.Description == "" && request.Timeout != nil {
		request.Description = "until " + request.Timeout.UTC().Format(extension.DueLayout)
	}
	after := request.AfterSequence
	if after == 0 {
		after = state.ObservedSequence
	}
	max, err := tx.LatestSequence()
	if err != nil {
		return nil, err
	}
	if after > max || after < state.ObservedSequence {
		return nil, extension.ErrInvalid
	}
	suffix := identity(string(x.ID), fmt.Sprint(state.Epoch))
	wait := domain.Wait{ID: domain.ID("wait/" + suffix), WorkID: w.ID, ExecutionID: x.ID, State: domain.WaitActive, Match: domain.EventMatch{Keys: append([]domain.CorrelationKey(nil), request.Match.Keys...), Types: append([]string(nil), request.Match.Types...)}, AfterSequence: after, Description: request.Description}
	decl := &ToolbeltDeclaration{Epoch: state.Epoch, Wait: &wait}
	if request.Timeout != nil {
		if !request.Timeout.After(now) || request.Timeout.Sub(now) > 365*24*time.Hour {
			return nil, extension.ErrInvalid
		}
		timerID := domain.ID("wait-timeout/" + suffix)
		wait.TimeoutTimerID = timerID
		event := domain.Event{ID: timerID, Type: "core.wait.timeout", Source: "core.timer", DeliveryID: string(timerID), CausationID: trigger, WorkID: w.ID, ExecutionID: x.ID, OccurredAt: *request.Timeout}
		decl.Timer = &domain.Timer{ID: timerID, FireAt: *request.Timeout, Event: event}
	}
	return decl, nil
}

// declarationInTx returns the current segment's toolbelt declaration, if any.
func declarationInTx(tx storage.Tx, ref domain.LeaseRef) (*ToolbeltDeclaration, error) {
	state, err := attemptState(tx, ref.ExecutionID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil || state.Declaration == nil || state.Declaration.Epoch != ref.Epoch {
		return nil, err
	}
	return state.Declaration, nil
}

// armDeclaredTimerInTx stores a declared wait timeout in the registration
// transaction so a failed registration cannot leave a stray timeout behind.
func armDeclaredTimerInTx(tx storage.Tx, now time.Time, scope domain.ID, timer domain.Timer) error {
	timer.Event.ReceivedAt = now
	if err := tx.PutTimer(timer, 0); err != nil {
		return err
	}
	return putJSON(tx, timerNamespace, timerKey(timer.ID), timerMetadata{ScopeID: scope})
}

var _ extension.Toolbelt = (*Toolbelt)(nil)
var _ extension.TokenIssuer = (*Toolbelt)(nil)
var _ extension.Registrar = (*Toolbelt)(nil)
