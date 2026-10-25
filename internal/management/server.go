// Package management serves the local authenticated management and toolbelt
// API (B05) over an owner-only Unix socket, calling generic runtime operations.
package management

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/internal/config"
	"github.com/alsiola/open-conduit/internal/runtime"
	"github.com/alsiola/open-conduit/storage"
)

const (
	idempotencyNamespace = "core.management.idempotency"
	maxBody              = 1 << 20
	idempotencyRetention = 30 * 24 * time.Hour
)

type Options struct {
	Store      storage.Store
	Artifacts  storage.Artifacts
	Now        func() time.Time
	Control    *runtime.Control
	Toolbelt   *runtime.Toolbelt
	Config     *ConfigService
	Version    string
	InstanceID string
	// Components supplies safe readiness diagnostics; nil reports ready.
	Components func() map[string]string
	CursorKey  []byte
	// Doctor inputs, all optional: ingress state for reconcile lag and loss
	// declarations, the scheduler for pinned-executor checks, a resolver for
	// explicit check_secrets runs and extension-contributed checks (telemetry
	// exporters, host watches, connection reachability).
	Ingress   *runtime.Ingress
	Scheduler *runtime.Scheduler
	Resolver  *config.Resolver
	Checks    []func(context.Context) runtime.Check
	// DeepChecks run only for check_secrets=true: they resolve credentials
	// and may call external services (connection authentication).
	DeepChecks []func(context.Context) runtime.Check
	// Annotate contributes extension context to GET /work/{id}.
	Annotate func(context.Context, domain.Work) []Annotation
	// Attach returns the attach command of the Work's latest attempt's
	// executor registration (ErrAttachUnsupported, ErrNoSession).
	Attach func(ctx context.Context, executor string, work domain.Work) (AttachSpec, error)
	// Nudge asks the host loop for an immediate pass (optional).
	Nudge func()
}

type Server struct {
	o Options
}

func NewServer(o Options) (*Server, error) {
	if o.Store == nil || o.Now == nil || o.Control == nil || o.Toolbelt == nil || o.Config == nil {
		return nil, storage.ErrInvalid
	}
	if len(o.CursorKey) == 0 {
		o.CursorKey = make([]byte, 32)
		if _, err := rand.Read(o.CursorKey); err != nil {
			return nil, err
		}
	}
	return &Server{o}, nil
}

type apiError struct {
	status  int
	code    string
	message string
	details []config.Diagnostic
}

func (e *apiError) Error() string { return e.code }
func fail(status int, code, message string) *apiError {
	return &apiError{status: status, code: code, message: message}
}

// mapError never discloses backend/provider error text.
var refusalCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func mapError(err error) *apiError {
	var api *apiError
	var invalid *config.ValidationError
	var refusal *extension.OperationError
	switch {
	case errors.As(err, &api):
		return api
	case errors.As(err, &refusal):
		code := refusal.Code
		if !refusalCode.MatchString(code) {
			code = "operation_refused"
		}
		msg := strings.ToValidUTF8(refusal.Message, "")
		if len(msg) > 500 {
			msg = strings.ToValidUTF8(msg[:500], "")
		}
		if strings.TrimSpace(msg) == "" {
			msg = "the operation refused the request"
		}
		return fail(422, code, msg)
	case errors.As(err, &invalid):
		return &apiError{http.StatusUnprocessableEntity, "invalid_config", "configuration is invalid", invalid.Diagnostics}
	case errors.Is(err, storage.ErrNotFound):
		return fail(404, "not_found", "target not found")
	case errors.Is(err, runtime.ErrRevision):
		return fail(409, "revision_conflict", "expected revision is stale")
	case errors.Is(err, runtime.ErrRequestConflict):
		return fail(409, "idempotency_conflict", "request identity reused with a different request")
	case errors.Is(err, ErrGenerationConflict):
		return fail(409, "generation_conflict", "expected generation is stale")
	case errors.Is(err, ErrScopeInUse):
		return fail(409, "scope_in_use", "scope has nonterminal work; drain it first")
	case errors.Is(err, runtime.ErrHeld):
		return fail(409, "hold_conflict", "the Work is already held, or not held")
	case errors.Is(err, ErrNoSession):
		return fail(409, "no_session", "the Work's executor has no session to attach to yet")
	case errors.Is(err, ErrAttachUnsupported):
		return fail(422, "attach_unsupported", "the Work's executor does not support attach")
	case errors.Is(err, ErrHashMismatch):
		return fail(422, "candidate_hash_mismatch", "candidate hash does not match the submitted config")
	case errors.Is(err, storage.ErrConflict), errors.Is(err, runtime.ErrNotEligible), errors.Is(err, storage.ErrClaimed):
		return fail(409, "state_conflict", "target state does not allow this operation")
	case errors.Is(err, runtime.ErrCausalRejected):
		return fail(429, "causal_limit", "causal depth or rate bound reached")
	case errors.Is(err, extension.ErrUnauthorized), errors.Is(err, storage.ErrLeaseLost):
		return fail(403, "forbidden", "execution authority rejected")
	case errors.Is(err, extension.ErrUndeclared):
		return fail(403, "undeclared", "operation or event family is not declared or allowed")
	case errors.Is(err, storage.ErrInvalid), errors.Is(err, extension.ErrInvalid), errors.Is(err, extension.ErrUnsupported):
		return fail(422, "invalid_request", "request is semantically invalid")
	}
	return fail(503, "unavailable", "storage or runtime unavailable; retry")
}

type principal struct {
	operator bool
	tokenID  string
	token    string
}

func requestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rid := requestID()
	w.Header().Set("X-Request-Id", rid)
	if err := s.serve(w, r); err != nil {
		api := mapError(err)
		body := map[string]any{"code": api.code, "message": api.message, "request_id": rid}
		if len(api.details) > 0 {
			details := make([]map[string]string, 0, len(api.details))
			for _, d := range api.details {
				details = append(details, map[string]string{"path": d.Path, "code": d.Code, "message": d.Message})
			}
			body["details"] = details
		}
		writeJSON(w, api.status, map[string]any{"error": body})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) authenticate(ctx context.Context, r *http.Request) (principal, error) {
	header := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" || len(token) > 256 {
		return principal{}, fail(401, "unauthenticated", "missing or invalid bearer token")
	}
	digest := runtime.TokenDigest(token)
	var record operatorRecord
	found := false
	err := s.o.Store.Transact(ctx, storage.Guard{Now: s.o.Now()}, func(tx storage.Tx) error {
		b, err := tx.Get(authNamespace, "operator/"+digest)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return json.Unmarshal(b, &record)
	})
	if err != nil {
		return principal{}, err
	}
	if found {
		if record.Revoked {
			return principal{}, fail(401, "unauthenticated", "credential revoked")
		}
		return principal{operator: true, tokenID: record.TokenID, token: token}, nil
	}
	known, err := s.o.Toolbelt.Known(ctx, token)
	if err != nil {
		return principal{}, err
	}
	if !known {
		return principal{}, fail(401, "unauthenticated", "missing or invalid bearer token")
	}
	return principal{token: token}, nil
}

type route struct {
	method   string
	pattern  []string // "*" captures one segment
	mutation bool
	idem     bool
	handler  func(*Server, *call) (int, any, error)
}

type call struct {
	ctx     context.Context
	r       *http.Request
	w       http.ResponseWriter
	who     principal
	params  []string
	body    []byte
	key     string
	written bool
}

var routes = []route{
	{"GET", []string{"health"}, false, false, (*Server).health},
	{"GET", []string{"schema"}, false, false, (*Server).schema},
	{"GET", []string{"scopes"}, false, false, (*Server).listScopes},
	{"GET", []string{"scopes", "*"}, false, false, (*Server).getScope},
	{"GET", []string{"work"}, false, false, (*Server).listWork},
	{"GET", []string{"work", "*"}, false, false, (*Server).getWork},
	{"GET", []string{"events"}, false, false, (*Server).listEvents},
	{"GET", []string{"events", "*"}, false, false, (*Server).getEvent},
	{"GET", []string{"executions"}, false, false, (*Server).listExecutions},
	{"GET", []string{"executions", "*"}, false, false, (*Server).getExecution},
	{"GET", []string{"waits"}, false, false, (*Server).listWaits},
	{"GET", []string{"waits", "*"}, false, false, (*Server).getWait},
	{"GET", []string{"runs"}, false, false, (*Server).listRuns},
	{"GET", []string{"runs", "*"}, false, false, (*Server).getRun},
	{"GET", []string{"doctor"}, false, false, (*Server).doctor},
	{"GET", []string{"artifacts"}, false, false, (*Server).listArtifacts},
	{"GET", []string{"artifacts", "*", "content"}, false, false, (*Server).artifactContent},
	{"POST", []string{"events"}, true, true, (*Server).postEvent},
	{"POST", []string{"work"}, true, true, (*Server).postWork},
	{"POST", []string{"work", "*", "cancel"}, true, true, (*Server).cancelWork},
	{"POST", []string{"work", "*", "retry"}, true, true, (*Server).retryWork},
	{"POST", []string{"work", "*", "wake"}, true, true, (*Server).wakeWork},
	{"POST", []string{"work", "*", "rebind"}, true, true, (*Server).rebindWork},
	{"POST", []string{"work", "*", "hold"}, true, true, (*Server).holdWork},
	{"POST", []string{"work", "*", "release"}, true, true, (*Server).releaseWork},
	{"POST", []string{"work", "*", "attach"}, true, true, (*Server).attachWork},
	{"POST", []string{"scopes", "*", "reconcile"}, true, true, (*Server).reconcileScope},
	{"POST", []string{"executions", "*", "cancel"}, true, true, (*Server).cancelExecution},
	{"POST", []string{"config", "validate"}, true, false, (*Server).validateConfig},
	{"POST", []string{"config", "apply"}, true, true, (*Server).applyConfig},
	{"POST", []string{"auth", "rotate"}, true, false, (*Server).rotate},
	{"POST", []string{"auth", "revoke"}, true, true, (*Server).revoke},
	{"POST", []string{"toolbelt", "call"}, true, false, (*Server).toolbeltCall},
}

// split matches routes on escaped segments so opaque IDs may contain "/".
func split(escaped string) ([]string, bool) {
	rest, ok := strings.CutPrefix(escaped, "/v1/")
	if !ok || rest == "" {
		return nil, false
	}
	parts := strings.Split(rest, "/")
	for i, p := range parts {
		value, err := url.PathUnescape(p)
		if err != nil || value == "" || value == "." || value == ".." || strings.ContainsAny(value, "\x00") {
			return nil, false
		}
		parts[i] = value
	}
	return parts, true
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	who, err := s.authenticate(ctx, r)
	if err != nil {
		return err
	}
	parts, ok := split(r.URL.EscapedPath())
	if !ok {
		return fail(404, "not_found", "unknown endpoint")
	}
	var matched *route
	var params []string
	methodSeen := false
	for i := range routes {
		rt := &routes[i]
		if len(rt.pattern) != len(parts) {
			continue
		}
		var captured []string
		match := true
		for j, p := range rt.pattern {
			if p == "*" {
				captured = append(captured, parts[j])
			} else if p != parts[j] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		methodSeen = true
		if rt.method == r.Method {
			matched, params = rt, captured
			break
		}
	}
	if matched == nil {
		if methodSeen {
			return fail(405, "method_not_allowed", "method not allowed")
		}
		return fail(404, "not_found", "unknown endpoint")
	}
	isToolbelt := matched.pattern[0] == "toolbelt"
	// Credential classes never cross: execution tokens reach only the toolbelt.
	if isToolbelt == who.operator {
		return fail(403, "wrong_credential_class", "credential class not permitted for this endpoint")
	}
	c := &call{ctx: ctx, r: r, w: w, who: who, params: params}
	if matched.mutation {
		if r.URL.RawQuery != "" {
			return fail(400, "query_not_allowed", "mutation routes accept no query")
		}
		if mt := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]); mt != "application/json" {
			return fail(415, "unsupported_media_type", "Content-Type must be application/json")
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil {
			return fail(400, "malformed", "request body unreadable")
		}
		if len(body) > maxBody {
			return fail(413, "body_too_large", "request body exceeds 1 MiB")
		}
		c.body = body
		c.key = r.Header.Get("Idempotency-Key")
		if c.key != "" && !validKey(c.key) {
			return fail(400, "invalid_idempotency_key", "Idempotency-Key must be 1-128 printable ASCII characters")
		}
		if matched.idem {
			if c.key == "" {
				return fail(400, "idempotency_key_required", "Idempotency-Key header is required")
			}
			return s.idempotent(c, matched)
		}
	}
	status, data, err := matched.handler(s, c)
	if err != nil {
		return err
	}
	if !c.written {
		writeJSON(w, status, map[string]any{"data": data})
	}
	return nil
}

func validKey(k string) bool {
	if len(k) < 1 || len(k) > 128 {
		return false
	}
	for _, ch := range k {
		if ch < 0x21 || ch > 0x7e {
			return false
		}
	}
	return true
}

type idemRecord struct {
	Hash      string
	Status    int
	Body      json.RawMessage
	ExpiresAt time.Time
}

// idempotent binds the key to principal class, method, path and canonical
// request hash. Handlers derive durable identities from the key, so replaying
// an interrupted request completes the same operation rather than repeating it.
func (s *Server) idempotent(c *call, rt *route) error {
	canonical, err := canonicalJSON(c.body)
	if err != nil {
		return fail(400, "malformed", "request body is not valid JSON")
	}
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])
	principalID := "operator"
	slot := hex.EncodeToString(func() []byte {
		h := sha256.Sum256([]byte(principalID + "\x00" + c.r.Method + "\x00" + c.r.URL.Path + "\x00" + c.key))
		return h[:]
	}())
	var prior *idemRecord
	if err = s.o.Store.Transact(c.ctx, storage.Guard{Now: s.o.Now()}, func(tx storage.Tx) error {
		b, e := tx.Get(idempotencyNamespace, slot)
		if errors.Is(e, storage.ErrNotFound) {
			return nil
		}
		if e != nil {
			return e
		}
		prior = &idemRecord{}
		return json.Unmarshal(b, prior)
	}); err != nil {
		return err
	}
	if prior != nil && s.o.Now().Before(prior.ExpiresAt) {
		if prior.Hash != hash {
			return fail(409, "idempotency_conflict", "Idempotency-Key reused with a different request")
		}
		c.w.Header().Set("Idempotency-Expires-At", ts(prior.ExpiresAt))
		c.w.Header().Set("Content-Type", "application/json")
		c.w.WriteHeader(prior.Status)
		_, _ = c.w.Write(prior.Body)
		return nil
	}
	status, data, err := rt.handler(s, c)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	record := idemRecord{Hash: hash, Status: status, Body: body, ExpiresAt: s.o.Now().Add(idempotencyRetention)}
	b, _ := json.Marshal(record)
	if err = s.o.Store.Transact(c.ctx, storage.Guard{Now: s.o.Now()}, func(tx storage.Tx) error { return tx.Put(idempotencyNamespace, slot, b) }); err != nil {
		return err
	}
	c.w.Header().Set("Idempotency-Expires-At", ts(record.ExpiresAt))
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(status)
	_, _ = c.w.Write(body)
	return nil
}

func canonicalJSON(b []byte) ([]byte, error) {
	var v any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// checkDuplicates rejects duplicate object keys at any depth.
func checkDuplicates(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var walk func() error
	walk = func() error {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				key := k.(string)
				if seen[key] {
					return fmt.Errorf("duplicate key")
				}
				seen[key] = true
				if err = walk(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		case json.Delim('['):
			for d.More() {
				if err = walk(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing data")
	}
	return nil
}

// decodeStrict rejects unknown fields, duplicate keys, explicit nulls at the
// top level and trailing data.
func decodeStrict(b []byte, v any) error {
	if err := checkDuplicates(b); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for _, value := range raw {
		if string(value) == "null" {
			return fmt.Errorf("null value")
		}
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func (c *call) decode(v any) error {
	if err := decodeStrict(c.body, v); err != nil {
		return fail(400, "malformed", "request body is malformed or has unknown, duplicate or null fields")
	}
	return nil
}

func parseRevision(s string) (domain.Revision, error) {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || s == "" {
		return 0, fail(400, "malformed", "revisions are unsigned decimal strings")
	}
	return domain.Revision(n), nil
}

// ---- list helpers ----

type cursor struct {
	Endpoint string `json:"e"`
	Filters  string `json:"f"`
	Bound    string `json:"b"`
	Last     string `json:"l"`
	Expires  int64  `json:"x"`
}

func (s *Server) sign(c cursor) string {
	b, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, s.o.CursorKey)
	mac.Write(b)
	return base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Server) open(token string) (cursor, error) {
	var c cursor
	body, sig, ok := strings.Cut(token, ".")
	b, err1 := base64.RawURLEncoding.DecodeString(body)
	m, err2 := base64.RawURLEncoding.DecodeString(sig)
	if !ok || err1 != nil || err2 != nil {
		return c, fail(400, "invalid_cursor", "cursor is invalid")
	}
	mac := hmac.New(sha256.New, s.o.CursorKey)
	mac.Write(b)
	if !hmac.Equal(m, mac.Sum(nil)) || json.Unmarshal(b, &c) != nil {
		return c, fail(400, "invalid_cursor", "cursor is invalid")
	}
	if s.o.Now().Unix() > c.Expires {
		return c, fail(410, "cursor_expired", "cursor expired; restart traversal")
	}
	return c, nil
}

type listQuery struct {
	limit   int
	filters map[string]string
	cursor  *cursor
	norm    string
}

func (s *Server) query(c *call, endpoint string, allowed ...string) (listQuery, error) {
	q := listQuery{limit: 50, filters: map[string]string{}}
	values := c.r.URL.Query()
	permitted := map[string]bool{"limit": true, "cursor": true}
	for _, a := range allowed {
		permitted[a] = true
	}
	var names []string
	for k, v := range values {
		if !permitted[k] || len(v) != 1 {
			return q, fail(400, "invalid_filter", "unknown or repeated query parameter")
		}
		if k != "limit" && k != "cursor" {
			q.filters[k] = v[0]
			names = append(names, k+"="+v[0])
		}
	}
	sort.Strings(names)
	q.norm = strings.Join(names, "&")
	if l := values.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 200 {
			return q, fail(400, "invalid_filter", "limit must be 1-200")
		}
		q.limit = n
	}
	if token := values.Get("cursor"); token != "" {
		cur, err := s.open(token)
		if err != nil {
			return q, err
		}
		if cur.Endpoint != endpoint || cur.Filters != q.norm {
			return q, fail(400, "invalid_cursor", "cursor does not match this traversal")
		}
		q.cursor = &cur
	}
	return q, nil
}

type keyed struct {
	key  string
	item any
}

// page applies keyset pagination over (created_at,id)-ordered keys with a
// captured upper bound for the traversal.
func (s *Server) page(endpoint string, q listQuery, bound string, items []keyed) map[string]any {
	sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })
	if q.cursor != nil {
		bound = q.cursor.Bound
	}
	out := []any{}
	next, last := "", ""
	for _, it := range items {
		if it.key > bound || q.cursor != nil && it.key <= q.cursor.Last {
			continue
		}
		if len(out) == q.limit {
			next = s.sign(cursor{Endpoint: endpoint, Filters: q.norm, Bound: bound, Last: last, Expires: s.o.Now().Add(15 * time.Minute).Unix()})
			break
		}
		out = append(out, it.item)
		last = it.key
	}
	return map[string]any{"items": out, "next_cursor": next}
}

func createdKey(t time.Time, id domain.ID) string {
	return fmt.Sprintf("%020d/%s", t.UnixNano(), id)
}
func (s *Server) nowBound() string { return fmt.Sprintf("%020d/\xff", s.o.Now().UnixNano()) }

func (s *Server) read(ctx context.Context, fn func(storage.Tx) error) error {
	return s.o.Store.Transact(ctx, storage.Guard{Now: s.o.Now()}, fn)
}

func etag(c *call, revision domain.Revision) bool {
	tag := fmt.Sprintf("\"%d\"", revision)
	c.w.Header().Set("ETag", tag)
	if c.r.Header.Get("If-None-Match") == tag {
		c.w.WriteHeader(http.StatusNotModified)
		c.written = true
		return true
	}
	return false
}

func noQuery(c *call) error {
	if c.r.URL.RawQuery != "" {
		return fail(400, "invalid_filter", "this endpoint accepts no query")
	}
	return nil
}
