package management

import (
	"context"
	"strconv"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// The audit ledger (storage.Tx.AppendAudit) records every administrative
// call: who made it, what it acted on and how it ended. The host writes
// it, never a handler's caller:
//
//   - A call that changes coordination state records its success in the
//     transaction that makes the change (auditInTx): a config or include
//     apply, issuing, rotating or revoking a credential, trusting or
//     removing an issuer. A control or action records it with the Event it
//     makes, in the runtime (eventAudited).
//   - A call whose change lives elsewhere (a secret in the keyring, a
//     release's binaries, an attach command) records its success once it
//     returns (auditAfter).
//   - Any audited call an operator makes that fails or is refused, and any
//     operator request whose role, scope or subject falls short, whatever
//     the route, records its refusal's code once it is refused. A request
//     whose credential is refused records that refusal (401), within a
//     budget per client.
//
// Reads and the calls that change nothing (route trace, config validate)
// are not recorded unless refused.

// eventAudited are the routes whose success the runtime records in the
// ledger with the Event it accepts for them (runtime.auditAccepted).
var eventAudited = map[string]bool{
	"events": true, "work": true,
	"work/*/cancel": true, "work/*/retry": true, "work/*/wake": true, "work/*/rebind": true,
	"work/*/hold": true, "work/*/interrupt": true, "work/*/release": true,
	"work/*/actions/*/*": true, "scopes/*/actions/*/*": true, "scopes/*/reconcile": true,
	"executions/*/cancel": true, "events/*/skip": true, "events/*/reroute": true,
}

// audited reports whether the route's calls are recorded in the ledger when
// they succeed or fail: every operator mutation but those that change
// nothing, and never the toolbelt (an execution's calls, which
// toolbelt.called records).
func audited(rt *route) bool {
	if rt == nil {
		return false
	}
	if strings.Join(rt.pattern, "/") == "work/*/terminal" {
		return true
	}
	if !rt.mutation || rt.pattern[0] == "toolbelt" {
		return false
	}
	switch strings.Join(rt.pattern, "/") {
	case "scopes/*/route/trace", "config/validate":
		return false
	}
	return true
}

// auditAction is how the ledger names a route: its method and its path as
// the HTTP API reference lists it ("POST /v1/work/{id}/cancel").
func auditAction(rt *route) string {
	if rt == nil {
		return "unknown"
	}
	return rt.method + " " + routePath(rt)
}

// auditTarget is what a call acts on: its path up to the first ID it
// names ("work/w-1", "secrets/github-token", "auth/issuers/okta"), or the
// target its handler set (the credential a revoke names); empty for the
// daemon as a whole.
func (c *call) auditTarget() string {
	if c.target != "" {
		return c.target
	}
	if c.rt == nil || len(c.params) == 0 {
		return ""
	}
	var parts []string
	for i, p := range c.rt.pattern {
		if p == "*" {
			// A refused request's path is whatever its caller sent: keep
			// at most an ID's length of it.
			return strings.ToValidUTF8(truncate(strings.Join(append(parts, c.params[0]), "/"), maxAuditTarget), "")
		}
		parts = append(parts, c.rt.pattern[i])
	}
	return ""
}

// maxAuditTarget bounds a target taken from a request's path.
const maxAuditTarget = 300

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// auditRecord is the ledger record of c's call with outcome: its actor and
// client as the host authenticated them, its route and target, and the
// facts its handler noted.
func (c *call) auditRecord(outcome string) domain.AuditRecord {
	r := domain.AuditRecord{Action: auditAction(c.rt), Target: c.auditTarget(), Outcome: outcome, Detail: c.detail}
	if c.who.operator {
		r.Actor, r.Via = c.who.actor(), c.who.via()
	}
	return r
}

// note adds a fact to the call's audit record. Values are never secret: a
// generation, a name, a credential's ID, never a token or a value.
func (c *call) note(key, value string) {
	if value == "" {
		return
	}
	if c.detail == nil {
		c.detail = map[string]string{}
	}
	c.detail[key] = value
}

// auditInTx records c's success in tx, the transaction that makes its
// change, so the change and its record commit together or not at all.
func (s *Server) auditInTx(tx storage.Tx, c *call) error {
	if _, err := tx.AppendAudit(c.auditRecord(domain.AuditOK)); err != nil {
		return err
	}
	c.recorded = true
	return nil
}

// auditAfter records r in a transaction of its own, for a call whose
// outcome is already settled: refused, failed, or done somewhere storage
// can't join. The call's answer stands whatever happens here; a record
// that can't be written is reported as the audit_unrecorded diagnostic.
func (s *Server) auditAfter(ctx context.Context, r domain.AuditRecord) {
	err := s.o.Store.Transact(context.WithoutCancel(ctx), storage.Guard{Now: s.o.Now()}, func(tx storage.Tx) error {
		_, err := tx.AppendAudit(r)
		return err
	})
	if err != nil && s.o.Diagnostic != nil {
		s.o.Diagnostic("audit_unrecorded")
	}
}

// auditRefusal records a request refused before or by its handler.
//
//   - A credential refused (401, on any route, the toolbelt's too) is
//     recorded as that refusal, and only within the client's budget:
//     AuthFailureLimit records per AuthFailureWindow for each network
//     client, execution socket and the owner's socket, with as many
//     clients tracked as the listener's throttle tracks. Someone with no
//     credential cannot otherwise write to a ledger that is never deleted,
//     and a throttled request (429) is never recorded.
//   - A request that authenticated an operator is recorded when its role,
//     scope or subject falls short (403, any route but the toolbelt's),
//     when its credential class is wrong, or when an audited call fails.
//
// Other failures, and every failure of a request that authenticated no
// operator, are not recorded.
func (s *Server) auditRefusal(c *call, err error) {
	if err == nil || c.recorded {
		return
	}
	api := mapError(err)
	toolbelt := c.rt != nil && c.rt.pattern[0] == "toolbelt"
	switch {
	case api.status == 401:
		if wait, _ := s.unauthenticated.fail(auditClient(c.ctx)); wait > 0 {
			return
		}
	case !c.who.operator:
		// Nothing a request that authenticated no operator does beyond
		// its 401 reaches the ledger: an execution's toolbelt refusals are
		// its own run's, which toolbelt.rejected records.
		return
	case api.code == "wrong_credential_class", api.status == 403 && !toolbelt, audited(c.rt):
	default:
		return
	}
	r := c.auditRecord(api.code)
	if api.status == 401 {
		// Nothing authenticated: say which credential was refused when the
		// host knows it (a revoked or expired one it issued), never the
		// token itself.
		r.Actor, r.Via = "", ""
		r.Detail = map[string]string{"reason": api.message}
		if c.who.tokenID != "" {
			r.Detail["credential"] = c.who.tokenID
			r.Detail["operator"] = extension.CallerOperator + ":" + c.who.name
		}
	}
	s.auditAfter(c.ctx, r)
}

// auditClient is who a refused credential's audit budget is counted
// against: the network client, the execution whose socket the request came
// in on, or the owner's socket.
func auditClient(ctx context.Context) string {
	if rc := remote(ctx); rc != nil {
		return rc.client
	}
	if id := scopedExecution(ctx); id != "" {
		return "execution:" + string(id)
	}
	return "socket"
}

// audit is GET /audit (admin): the audit ledger in sequence order,
// exclusively after the after sequence, at most limit records (default and
// most 1000). An exporter streams it to a SIEM by asking again after the
// last sequence it holds, and checks the chain as it goes
// (domain.VerifyAuditChain); next_after is where to ask from next.
func (s *Server) audit(c *call) (int, any, error) {
	q := c.r.URL.Query()
	for k := range q {
		if k != "after" && k != "limit" {
			return 0, nil, fail(400, "unknown_filter", "audit takes after and limit")
		}
	}
	var after uint64
	if v := q.Get("after"); v != "" {
		var err error
		if after, err = strconv.ParseUint(v, 10, 64); err != nil {
			return 0, nil, fail(400, "malformed", "after must be a decimal sequence")
		}
	}
	limit := maxAuditPage
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxAuditPage {
			return 0, nil, fail(400, "malformed", "limit must be 1 to 1000")
		}
		limit = n
	}
	var records []domain.AuditRecord
	if err := s.read(c.ctx, func(tx storage.Tx) error {
		var err error
		records, err = tx.AuditRecords(after, limit)
		return err
	}); err != nil {
		return 0, nil, err
	}
	out := make([]wireAudit, 0, len(records))
	next := after
	for _, r := range records {
		out = append(out, toWireAudit(r))
		next = r.Sequence
	}
	return 200, map[string]any{"records": out, "next_after": u64(next)}, nil
}

const maxAuditPage = 1000

// wireAudit is an audit record on the wire. sequence is a decimal string,
// as generations are, so no client loses precision.
type wireAudit struct {
	Sequence string            `json:"sequence"`
	At       string            `json:"at"`
	Actor    string            `json:"actor,omitempty"`
	Via      string            `json:"via,omitempty"`
	Action   string            `json:"action"`
	Target   string            `json:"target,omitempty"`
	Outcome  string            `json:"outcome"`
	EventID  string            `json:"event_id,omitempty"`
	Detail   map[string]string `json:"detail,omitempty"`
	Prev     string            `json:"prev,omitempty"`
	Hash     string            `json:"hash"`
}

func toWireAudit(r domain.AuditRecord) wireAudit {
	return wireAudit{Sequence: u64(r.Sequence), At: ts(r.At), Actor: r.Actor, Via: r.Via, Action: r.Action, Target: r.Target, Outcome: r.Outcome, EventID: string(r.EventID), Detail: r.Detail, Prev: r.Prev, Hash: r.Hash}
}

// configRecorder records a config or include apply in the transaction that
// publishes its generation, with the generation's number and hash: the
// generation's stored text then says what the apply changed.
func (s *Server) configRecorder(c *call) Recorder {
	return func(tx storage.Tx, generation uint64, hash string) error {
		c.note("generation", u64(generation))
		c.note("hash", hash)
		return s.auditInTx(tx, c)
	}
}

// grantsNote is grants as one fact: scope=role, comma-separated.
func grantsNote(grants []Grant) string {
	parts := make([]string, 0, len(grants))
	for _, g := range grants {
		parts = append(parts, string(g.Scope)+"="+g.Role)
	}
	return strings.Join(parts, ",")
}
