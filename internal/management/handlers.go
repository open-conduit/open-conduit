package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

func (s *Server) health(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	_, generation := s.o.Config.Current()
	components := map[string]string{"database": "ready"}
	if err := s.read(c.ctx, func(tx storage.Tx) error { _, e := tx.ListScopes(); return e }); err != nil {
		components["database"] = "unavailable"
	}
	state := "ready"
	if s.o.Components != nil {
		for k, v := range s.o.Components() {
			components[k] = v
		}
	}
	for _, v := range components {
		if v != "ready" {
			state = "degraded"
		}
	}
	return 200, map[string]any{"state": state, "version": s.o.Version, "instance_id": s.o.InstanceID, "config_generation": u64(generation), "components": components}, nil
}

// Pool is one capacity pool's slots: the limit from settings (zero:
// unbounded), the attempts holding a slot and the ready Work whose last
// start this pool's limit held back.
type Pool struct {
	Name    string `json:"name"`
	Setting string `json:"setting,omitempty"`
	Limit   int    `json:"limit"`
	InUse   int    `json:"in_use"`
	Waiting int    `json:"waiting"`
}

func (s *Server) capacity(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	pools := []Pool{}
	if s.o.Capacity != nil {
		got, err := s.o.Capacity(c.ctx)
		if err != nil {
			return 0, nil, err
		}
		pools = append(pools, got...)
	}
	return 200, map[string]any{"pools": pools}, nil
}

func (s *Server) schema(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"api_version": "1", "contract_version": strconv.Itoa(int(extension.ContractVersion)), "operations": s.o.Toolbelt.Operations(), "event_types": s.o.Toolbelt.EventTypes()}, nil
}

// ---- scopes ----

func (s *Server) listScopes(c *call) (int, any, error) {
	q, err := s.query(c, "scopes", "parent_id", "enabled")
	if err != nil {
		return 0, nil, err
	}
	if e, ok := q.filters["enabled"]; ok && e != "true" && e != "false" {
		return 0, nil, fail(400, "invalid_filter", "enabled must be true or false")
	}
	var items []keyed
	err = s.read(c.ctx, func(tx storage.Tx) error {
		scopes, e := tx.ListScopes()
		for _, sc := range scopes {
			enabled := s.o.Config.Enabled(sc.ID)
			if p, ok := q.filters["parent_id"]; ok && string(sc.ParentID) != p {
				continue
			}
			if v, ok := q.filters["enabled"]; ok && strconv.FormatBool(enabled) != v {
				continue
			}
			items = append(items, keyed{createdKey(sc.CreatedAt, sc.ID), toScope(sc, enabled)})
		}
		return e
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, s.page("scopes", q, s.nowBound(), items), nil
}
func (s *Server) getScope(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	var sc domain.Scope
	if err := s.read(c.ctx, func(tx storage.Tx) error { var e error; sc, e = tx.Scope(domain.ID(c.params[0])); return e }); err != nil {
		return 0, nil, err
	}
	if etag(c, sc.Revision) {
		return 0, nil, nil
	}
	return 200, toScope(sc, s.o.Config.Enabled(sc.ID)), nil
}

// ---- work ----

func (s *Server) listWork(c *call) (int, any, error) {
	q, err := s.query(c, "work", "scope_id", "state", "kind", "parent_id")
	if err != nil {
		return 0, nil, err
	}
	var items []keyed
	err = s.read(c.ctx, func(tx storage.Tx) error {
		works, e := tx.ListWorks()
		for _, w := range works {
			if !match(q, "scope_id", string(w.ScopeID)) || !match(q, "state", string(w.State)) || !match(q, "kind", w.Kind) || !match(q, "parent_id", string(w.ParentID)) {
				continue
			}
			items = append(items, keyed{createdKey(w.CreatedAt, w.ID), toWork(w)})
		}
		return e
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, s.page("work", q, s.nowBound(), items), nil
}
func match(q listQuery, name, value string) bool {
	want, ok := q.filters[name]
	return !ok || want == value
}
func (s *Server) getWork(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	var w domain.Work
	if err := s.read(c.ctx, func(tx storage.Tx) error { var e error; w, e = tx.Work(domain.ID(c.params[0])); return e }); err != nil {
		return 0, nil, err
	}
	if etag(c, w.Revision) {
		return 0, nil, nil
	}
	d, err := s.detail(c.ctx, w)
	if err != nil {
		return 0, nil, err
	}
	return 200, d, nil
}

// ---- events ----

func (s *Server) listEvents(c *call) (int, any, error) {
	q, err := s.query(c, "events", "scope_id", "work_id", "type", "after_sequence")
	if err != nil {
		return 0, nil, err
	}
	var after uint64
	if v, ok := q.filters["after_sequence"]; ok {
		if after, err = strconv.ParseUint(v, 10, 64); err != nil {
			return 0, nil, fail(400, "invalid_filter", "after_sequence must be a decimal string")
		}
	}
	out := []any{}
	next := ""
	err = s.read(c.ctx, func(tx storage.Tx) error {
		bound := uint64(0)
		if q.cursor != nil {
			last, _ := strconv.ParseUint(q.cursor.Last, 10, 64)
			bound, _ = strconv.ParseUint(q.cursor.Bound, 10, 64)
			if last > after {
				after = last
			}
		} else {
			var e error
			if bound, e = tx.LatestSequence(); e != nil {
				return e
			}
		}
		for {
			batch, e := tx.Events(after, 200)
			if e != nil {
				return e
			}
			if len(batch) == 0 {
				return nil
			}
			for _, a := range batch {
				if a.Sequence > bound {
					return nil
				}
				after = a.Sequence
				scope, e := runtime.EventScopeInTx(tx, a)
				if e != nil {
					return e
				}
				if !match(q, "scope_id", string(scope)) || !match(q, "type", a.Event.Type) || !(match(q, "work_id", string(a.Event.WorkID)) || match(q, "work_id", string(a.RoutedWorkID))) {
					continue
				}
				if len(out) == q.limit {
					next = s.sign(cursor{Endpoint: "events", Filters: q.norm, Bound: u64(bound), Last: u64(a.Sequence - 1), Expires: s.o.Now().Add(15 * time.Minute).Unix()})
					return nil
				}
				out = append(out, toEvent(a, scope))
			}
		}
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"items": out, "next_cursor": next}, nil
}
func (s *Server) getEvent(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	var out wireEvent
	err := s.read(c.ctx, func(tx storage.Tx) error {
		a, e := tx.Event(domain.ID(c.params[0]))
		if e != nil {
			return e
		}
		scope, e := runtime.EventScopeInTx(tx, a)
		out = toEvent(a, scope)
		return e
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, out, nil
}

// eventRoute explains how an accepted Event was routed: its disposition and
// the durable route decision (route, binding, generation, policy, reason).
func (s *Server) eventRoute(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	var out wireEventRoute
	err := s.read(c.ctx, func(tx storage.Tx) error {
		a, e := tx.Event(domain.ID(c.params[0]))
		if e != nil {
			return e
		}
		scope, e := runtime.EventScopeInTx(tx, a)
		if e != nil {
			return e
		}
		rec, ok, e := runtime.RouteDecisionInTx(tx, a.Event.ID)
		if e != nil {
			return e
		}
		out = wireEventRoute{EventID: string(a.Event.ID), Type: a.Event.Type, ScopeID: string(scope), Routed: a.Routed, Disposition: a.Disposition, RoutedWorkID: string(a.RoutedWorkID)}
		if ok {
			out.Route, out.Binding, out.Generation, out.Policy, out.Reason = rec.Route, rec.Binding, rec.Generation, rec.Policy, rec.Reason
			out.DisplayRefused = rec.DisplayRefused
			if out.RoutedWorkID == "" {
				out.RoutedWorkID = string(rec.WorkID)
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, out, nil
}

// ---- executions ----

func (s *Server) listExecutions(c *call) (int, any, error) {
	q, err := s.query(c, "executions", "scope_id", "work_id", "state")
	if err != nil {
		return 0, nil, err
	}
	var items []keyed
	err = s.read(c.ctx, func(tx storage.Tx) error {
		xs, e := tx.ListExecutions(domain.ID(q.filters["work_id"]))
		if e != nil {
			return e
		}
		scopes := map[domain.ID]domain.ID{}
		for _, x := range xs {
			scope, ok := scopes[x.WorkID]
			if !ok {
				w, e := tx.Work(x.WorkID)
				if e != nil {
					return e
				}
				scope, scopes[x.WorkID] = w.ScopeID, w.ScopeID
			}
			if !match(q, "scope_id", string(scope)) || !match(q, "state", string(x.State)) || !match(q, "work_id", string(x.WorkID)) {
				continue
			}
			items = append(items, keyed{createdKey(x.CreatedAt, x.ID), toExecution(x, scope)})
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, s.page("executions", q, s.nowBound(), items), nil
}
func (s *Server) getExecution(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	var x domain.Execution
	var w domain.Work
	err := s.read(c.ctx, func(tx storage.Tx) error {
		var e error
		if x, e = tx.Execution(domain.ID(c.params[0])); e != nil {
			return e
		}
		w, e = tx.Work(x.WorkID)
		return e
	})
	if err != nil {
		return 0, nil, err
	}
	if etag(c, x.Revision) {
		return 0, nil, nil
	}
	return 200, toExecution(x, w.ScopeID), nil
}

// ---- waits ----

func (s *Server) listWaits(c *call) (int, any, error) {
	q, err := s.query(c, "waits", "scope_id", "work_id", "execution_id", "state")
	if err != nil {
		return 0, nil, err
	}
	var items []keyed
	err = s.read(c.ctx, func(tx storage.Tx) error {
		waits, e := tx.ListWaits(domain.ID(q.filters["work_id"]))
		if e != nil {
			return e
		}
		for _, wait := range waits {
			if _, ok := q.filters["scope_id"]; ok {
				w, e := tx.Work(wait.WorkID)
				if e != nil {
					return e
				}
				if !match(q, "scope_id", string(w.ScopeID)) {
					continue
				}
			}
			if !match(q, "execution_id", string(wait.ExecutionID)) || !match(q, "state", string(wait.State)) || !match(q, "work_id", string(wait.WorkID)) {
				continue
			}
			items = append(items, keyed{createdKey(wait.CreatedAt, wait.ID), toWait(wait, waitDue(tx, wait))})
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, s.page("waits", q, s.nowBound(), items), nil
}
func (s *Server) getWait(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	var wait domain.Wait
	var due time.Time
	if err := s.read(c.ctx, func(tx storage.Tx) error {
		var e error
		wait, e = tx.Wait(domain.ID(c.params[0]))
		due = waitDue(tx, wait)
		return e
	}); err != nil {
		return 0, nil, err
	}
	if etag(c, wait.Revision) {
		return 0, nil, nil
	}
	return 200, toWait(wait, due), nil
}

// waitDue is when a Wait's timeout fires: zero without a timer, or once
// the timer has gone.
func waitDue(tx storage.Tx, w domain.Wait) time.Time {
	if w.TimeoutTimerID == "" {
		return time.Time{}
	}
	t, err := tx.Timer(w.TimeoutTimerID)
	if err != nil {
		return time.Time{}
	}
	return t.FireAt
}

// ---- artifacts (logs/outputs) ----

func (s *Server) artifacts(c *call) ([]wireArtifact, []domain.Execution, error) {
	var out []wireArtifact
	var xs []domain.Execution
	err := s.read(c.ctx, func(tx storage.Tx) error {
		var e error
		xs, e = tx.ListExecutions("")
		for _, x := range xs {
			for _, a := range x.Artifacts {
				out = append(out, wireArtifact{ID: a.ID, Size: a.Size, MediaType: a.MediaType, WorkID: string(x.WorkID), ExecutionID: string(x.ID)})
			}
		}
		return e
	})
	return out, xs, err
}
func (s *Server) listArtifacts(c *call) (int, any, error) {
	q, err := s.query(c, "artifacts", "work_id", "execution_id", "kind")
	if err != nil {
		return 0, nil, err
	}
	all, _, err := s.artifacts(c)
	if err != nil {
		return 0, nil, err
	}
	var items []keyed
	for i, a := range all {
		if !match(q, "work_id", a.WorkID) || !match(q, "execution_id", a.ExecutionID) || !match(q, "kind", a.MediaType) {
			continue
		}
		items = append(items, keyed{fmt.Sprintf("%s/%010d", a.ExecutionID, i), a})
	}
	return 200, s.page("artifacts", q, "\xff", items), nil
}

var rangePattern = regexp.MustCompile(`^bytes=(\d*)-(\d*)$`)

func (s *Server) artifactContent(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	if s.o.Artifacts == nil {
		return 0, nil, storage.ErrNotFound
	}
	all, _, err := s.artifacts(c)
	if err != nil {
		return 0, nil, err
	}
	var ref *domain.ArtifactRef
	owner := ""
	for _, a := range all {
		if a.ID == c.params[0] {
			ref = &domain.ArtifactRef{ID: a.ID, Size: a.Size, MediaType: a.MediaType}
			owner = a.ExecutionID
			break
		}
	}
	if ref == nil {
		return 0, nil, storage.ErrNotFound
	}
	var retention runtime.RetentionStatus
	if err = s.read(c.ctx, func(tx storage.Tx) error {
		var e error
		retention, e = runtime.RetentionInTx(tx, domain.ID(owner))
		return e
	}); err != nil {
		return 0, nil, err
	}
	if retention.Expired {
		// Metadata stays in history; content is gone under the retention policy.
		return 0, nil, fail(404, "artifact_expired", "artifact content expired under the retention policy")
	}
	start, end := int64(0), ref.Size-1
	partial := false
	if h := c.r.Header.Get("Range"); h != "" {
		m := rangePattern.FindStringSubmatch(h)
		if m == nil || m[1] == "" && m[2] == "" {
			return 0, nil, fail(416, "range_not_satisfiable", "one standard byte range is supported")
		}
		if m[1] == "" {
			n, _ := strconv.ParseInt(m[2], 10, 64)
			start = ref.Size - n
			if start < 0 {
				start = 0
			}
		} else {
			start, _ = strconv.ParseInt(m[1], 10, 64)
			if m[2] != "" {
				end, _ = strconv.ParseInt(m[2], 10, 64)
			}
		}
		if end >= ref.Size {
			end = ref.Size - 1
		}
		if start >= ref.Size || start > end {
			c.w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", ref.Size))
			return 0, nil, fail(416, "range_not_satisfiable", "range outside content")
		}
		partial = true
	}
	r, err := s.o.Artifacts.Open(c.ctx, *ref)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return 0, nil, storage.ErrNotFound
		}
		return 0, nil, err
	}
	defer r.Close()
	if _, err = io.CopyN(io.Discard, r, start); err != nil {
		return 0, nil, err
	}
	c.w.Header().Set("Content-Type", "application/octet-stream")
	c.w.Header().Set("Accept-Ranges", "bytes")
	c.w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
		c.w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, ref.Size))
	}
	c.w.WriteHeader(status)
	c.written = true
	_, _ = io.CopyN(c.w, r, end-start+1)
	return status, nil, nil
}

// ---- mutations ----

type keyIn struct {
	Source     string `json:"source"`
	ExternalID string `json:"external_id"`
}
type payloadIn struct {
	Schema string          `json:"schema"`
	Data   json.RawMessage `json:"data"`
}

func (p *payloadIn) domain() domain.Payload {
	if p == nil {
		return domain.Payload{}
	}
	return domain.Payload{Schema: p.Schema, Data: p.Data}
}
func (k *keyIn) domain() *domain.CorrelationKey {
	if k == nil {
		return nil
	}
	return &domain.CorrelationKey{Source: k.Source, ExternalID: k.ExternalID}
}

func (s *Server) postEvent(c *call) (int, any, error) {
	var in struct {
		ScopeID        string     `json:"scope_id"`
		Type           string     `json:"type"`
		DeliveryID     string     `json:"delivery_id"`
		Data           *payloadIn `json:"data"`
		CorrelationKey *keyIn     `json:"correlation_key"`
		WorkID         string     `json:"work_id"`
		CausationID    string     `json:"causation_id"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	if in.ScopeID == "" {
		return 0, nil, fail(400, "malformed", "scope_id is required")
	}
	accepted, added, err := s.o.Control.SubmitEvent(c.ctx, runtime.ManualEvent{ScopeID: domain.ID(in.ScopeID), WorkID: domain.ID(in.WorkID), CausationID: domain.ID(in.CausationID), Type: in.Type, DeliveryID: in.DeliveryID, Data: in.Data.domain(), CorrelationKey: in.CorrelationKey.domain()})
	if err != nil {
		return 0, nil, err
	}
	status := 200
	if added {
		status = 201
	}
	return status, toEvent(accepted, domain.ID(in.ScopeID)), nil
}

func (s *Server) postWork(c *call) (int, any, error) {
	var in struct {
		ScopeID         string     `json:"scope_id"`
		Kind            string     `json:"kind"`
		Context         *payloadIn `json:"context"`
		CorrelationKeys []keyIn    `json:"correlation_keys"`
		ParentID        string     `json:"parent_id"`
		Binding         string     `json:"binding"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	var ks []domain.CorrelationKey
	for _, k := range in.CorrelationKeys {
		ks = append(ks, *k.domain())
	}
	w, created, err := s.o.Control.CreateWork(c.ctx, runtime.ManualWork{ScopeID: domain.ID(in.ScopeID), ParentID: domain.ID(in.ParentID), Kind: in.Kind, Binding: in.Binding, DeliveryID: c.key, Context: in.Context.domain(), Keys: ks})
	if err != nil {
		return 0, nil, err
	}
	return 201, map[string]any{"work": toWork(w), "event_id": string(created.Event.ID)}, nil
}

type controlIn struct {
	ExpectedRevision string `json:"expected_revision"`
	Reason           string `json:"reason"`
}

func (s *Server) control(c *call) (domain.Revision, string, error) {
	var in controlIn
	if err := c.decode(&in); err != nil {
		return 0, "", err
	}
	rev, err := parseRevision(in.ExpectedRevision)
	if len(in.Reason) > 1024 {
		return 0, "", fail(400, "malformed", "reason is too long")
	}
	return rev, in.Reason, err
}
func (s *Server) cancelWork(c *call) (int, any, error) {
	rev, reason, err := s.control(c)
	if err != nil {
		return 0, nil, err
	}
	event, err := s.o.Control.CancelWork(c.ctx, domain.ID(c.params[0]), rev, reason, c.key)
	if err != nil {
		return 0, nil, err
	}
	return 202, map[string]any{"event_id": string(event), "work_id": c.params[0]}, nil
}
func (s *Server) retryWork(c *call) (int, any, error) {
	rev, reason, err := s.control(c)
	if err != nil {
		return 0, nil, err
	}
	event, err := s.o.Control.RetryWork(c.ctx, domain.ID(c.params[0]), rev, reason, c.key)
	if err != nil {
		return 0, nil, err
	}
	return 202, map[string]any{"event_id": string(event), "work_id": c.params[0]}, nil
}
func (s *Server) cancelExecution(c *call) (int, any, error) {
	rev, reason, err := s.control(c)
	if err != nil {
		return 0, nil, err
	}
	event, work, err := s.o.Control.CancelExecution(c.ctx, domain.ID(c.params[0]), rev, reason, c.key)
	if err != nil {
		return 0, nil, err
	}
	return 202, map[string]any{"event_id": string(event), "work_id": string(work), "execution_id": c.params[0]}, nil
}
func (s *Server) wakeWork(c *call) (int, any, error) {
	var in struct {
		ExpectedRevision string     `json:"expected_revision"`
		Type             string     `json:"type"`
		DeliveryID       string     `json:"delivery_id"`
		Data             *payloadIn `json:"data"`
		CorrelationKey   *keyIn     `json:"correlation_key"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	rev, err := parseRevision(in.ExpectedRevision)
	if err != nil {
		return 0, nil, err
	}
	accepted, matched, err := s.o.Control.Wake(c.ctx, domain.ID(c.params[0]), rev, runtime.ManualEvent{Type: in.Type, DeliveryID: in.DeliveryID, Data: in.Data.domain(), CorrelationKey: in.CorrelationKey.domain()})
	if err != nil {
		return 0, nil, err
	}
	var scope domain.ID
	_ = s.read(c.ctx, func(tx storage.Tx) error { var e error; scope, e = runtime.EventScopeInTx(tx, accepted); return e })
	return 202, map[string]any{"event": toEvent(accepted, scope), "matched": matched}, nil
}

// config is the active generation's wiring: each scope's extensions, routes
// and bindings by name and type, and the connections they use, with what
// an extension or route handler says its config sets up (a schedule's
// jobs). It carries no opaque config and no secret references.
func (s *Server) config(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	snap, generation := s.o.Config.Current()
	reg := s.o.Config.Registry()
	out := wireConfig{Generation: u64(generation), Hash: snap.Hash, Connections: []wireConnection{}, Scopes: []wireConfigScope{}}
	for _, conn := range snap.Connections {
		out.Connections = append(out.Connections, wireConnection{ID: conn.ID, Type: conn.Type})
	}
	for _, id := range snap.Order {
		e := snap.Scopes[id]
		if e == nil {
			continue
		}
		sc := wireConfigScope{ID: string(e.ID), Name: e.Name, ParentID: string(e.ParentID), Enabled: e.Enabled, Source: e.Source, Extensions: []wireConfigExtension{}, Routes: []wireConfigRoute{}, Bindings: []wireConfigBinding{}}
		for _, x := range e.Extensions {
			t := reg.Extensions[x.Type]
			wx := wireConfigExtension{Name: x.Name, Type: x.Type, Connection: x.Connection, Ingress: t.Ingress}
			if t.Describe != nil {
				for _, en := range t.Describe(x.Config) {
					wx.Entries = append(wx.Entries, wireEntry{en.Name, en.Detail})
				}
			}
			sc.Extensions = append(sc.Extensions, wx)
		}
		for _, r := range e.Routes {
			wr := wireConfigRoute{wireRoute: wireRoute{r.Name, r.Handler, r.Binding, r.ActivePolicy}}
			if d := reg.HandlerDescribe[r.Handler]; d != nil {
				wr.Detail = d(r.Config)
			}
			sc.Routes = append(sc.Routes, wr)
		}
		for _, b := range e.Bindings {
			wb := wireConfigBinding{Name: b.Name, Executor: b.Executor, Connection: b.Connection}
			if b.Resource != nil {
				wb.Resource = b.Resource.Provider
			}
			sc.Bindings = append(sc.Bindings, wb)
		}
		out.Scopes = append(out.Scopes, sc)
	}
	return 200, out, nil
}

func (s *Server) validateConfig(c *call) (int, any, error) {
	var in struct {
		Config json.RawMessage `json:"config"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	snapshot, err := s.o.Config.Validate(in.Config)
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"valid": true, "candidate_hash": snapshot.Hash, "config": snapshot.Redacted()}, nil
}
func (s *Server) applyConfig(c *call) (int, any, error) {
	var in struct {
		Config             json.RawMessage `json:"config"`
		CandidateHash      string          `json:"candidate_hash"`
		ExpectedGeneration string          `json:"expected_generation"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	expected, err := strconv.ParseUint(in.ExpectedGeneration, 10, 64)
	if err != nil {
		return 0, nil, fail(400, "malformed", "expected_generation must be a decimal string")
	}
	generation, hash, err := s.o.Config.Apply(c.ctx, in.Config, in.CandidateHash, expected)
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"generation": u64(generation), "hash": hash}, nil
}

// rotate never stores the replacement in idempotency history; a lost response
// recovers through the locked local CLI (RecoverOperator).
func (s *Server) rotate(c *call) (int, any, error) {
	var in struct {
		ExpectedTokenID string `json:"expected_token_id"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	if in.ExpectedTokenID != c.who.tokenID {
		return 0, nil, fail(409, "token_conflict", "expected_token_id is not the calling credential")
	}
	var file TokenFile
	err := s.o.Store.Transact(c.ctx, storage.Guard{Now: s.o.Now()}, func(tx storage.Tx) error {
		digest := runtime.TokenDigest(c.who.token)
		b, err := tx.Get(authNamespace, "operator/"+digest)
		if err != nil {
			return err
		}
		var r operatorRecord
		if err = json.Unmarshal(b, &r); err != nil {
			return err
		}
		if r.Revoked {
			return fail(401, "unauthenticated", "credential revoked")
		}
		r.Revoked, r.RevokedAt = true, s.o.Now()
		b, _ = json.Marshal(r)
		if err = tx.Put(authNamespace, "operator/"+digest, b); err != nil {
			return err
		}
		file, err = issueOperatorInTx(tx, s.o.Now())
		return err
	})
	if err != nil {
		return 0, nil, err
	}
	c.w.Header().Set("Cache-Control", "no-store")
	return 200, map[string]any{"token_id": file.TokenID, "token": file.Token}, nil
}
func (s *Server) revoke(c *call) (int, any, error) {
	var in struct {
		TokenID string `json:"token_id"`
		Reason  string `json:"reason"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	if strings.HasPrefix(in.TokenID, "otok_") {
		if in.TokenID != c.who.tokenID {
			return 0, nil, storage.ErrNotFound
		}
		var at time.Time
		err := s.o.Store.Transact(c.ctx, storage.Guard{Now: s.o.Now()}, func(tx storage.Tx) error {
			digest := runtime.TokenDigest(c.who.token)
			b, err := tx.Get(authNamespace, "operator/"+digest)
			if err != nil {
				return err
			}
			var r operatorRecord
			if err = json.Unmarshal(b, &r); err != nil {
				return err
			}
			if !r.Revoked {
				r.Revoked, r.RevokedAt = true, s.o.Now()
			}
			at = r.RevokedAt
			b, _ = json.Marshal(r)
			return tx.Put(authNamespace, "operator/"+digest, b)
		})
		if err != nil {
			return 0, nil, err
		}
		return 200, map[string]any{"token_id": in.TokenID, "revoked_at": ts(at)}, nil
	}
	at, err := s.o.Toolbelt.RevokeID(c.ctx, in.TokenID)
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"token_id": in.TokenID, "revoked_at": ts(at)}, nil
}

// ---- toolbelt ----

func (s *Server) toolbeltCall(c *call) (int, any, error) {
	var in struct {
		Operation   string          `json:"operation"`
		Input       json.RawMessage `json:"input"`
		RequestID   string          `json:"request_id"`
		ScopeID     string          `json:"scope_id"`
		WorkID      string          `json:"work_id"`
		ExecutionID string          `json:"execution_id"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	if in.RequestID == "" {
		in.RequestID = c.key
	}
	call := extension.ToolCall{Operation: in.Operation, ScopeID: domain.ID(in.ScopeID), WorkID: domain.ID(in.WorkID), ExecutionID: domain.ID(in.ExecutionID)}
	input := in.Input
	if len(input) == 0 {
		input = []byte("{}")
	}
	strict := func(v any) error {
		if err := decodeStrict(input, v); err != nil {
			return fail(400, "malformed", "operation input is malformed or has unknown fields")
		}
		return nil
	}
	switch in.Operation {
	case extension.ReadContext:
		var empty struct{}
		if err := strict(&empty); err != nil {
			return 0, nil, err
		}
	case extension.Emit:
		var e struct {
			Type           string     `json:"type"`
			DeliveryID     string     `json:"delivery_id"`
			Data           *payloadIn `json:"data"`
			CorrelationKey *keyIn     `json:"correlation_key"`
			OccurredAt     string     `json:"occurred_at"`
		}
		if err := strict(&e); err != nil {
			return 0, nil, err
		}
		event := domain.Event{Type: e.Type, DeliveryID: e.DeliveryID, Data: e.Data.domain(), CorrelationKey: e.CorrelationKey.domain()}
		if e.OccurredAt != "" {
			t, err := time.Parse(time.RFC3339Nano, e.OccurredAt)
			if err != nil {
				return 0, nil, fail(400, "malformed", "occurred_at must be RFC3339")
			}
			event.OccurredAt = t.UTC()
		}
		call.Event = &event
	case extension.CreateWork:
		var w struct {
			ID              string     `json:"id"`
			Kind            string     `json:"kind"`
			CorrelationKeys []keyIn    `json:"correlation_keys"`
			Context         *payloadIn `json:"context"`
		}
		if err := strict(&w); err != nil {
			return 0, nil, err
		}
		child := domain.Work{ID: domain.ID(w.ID), Kind: w.Kind, Context: w.Context.domain()}
		for _, k := range w.CorrelationKeys {
			child.CorrelationKeys = append(child.CorrelationKeys, *k.domain())
		}
		call.Child = &child
	case extension.Wait:
		var w struct {
			Match struct {
				Keys  []keyIn  `json:"keys"`
				Types []string `json:"types"`
			} `json:"match"`
			TimeoutSeconds int64  `json:"timeout_seconds"`
			AfterSequence  string `json:"after_sequence"`
			Description    string `json:"description"`
		}
		if err := strict(&w); err != nil {
			return 0, nil, err
		}
		request := extension.WaitRequest{Match: domain.EventMatch{Types: w.Match.Types}, Description: w.Description}
		for _, k := range w.Match.Keys {
			request.Match.Keys = append(request.Match.Keys, *k.domain())
		}
		if w.TimeoutSeconds < 0 {
			return 0, nil, fail(400, "malformed", "timeout_seconds must be positive")
		}
		if w.TimeoutSeconds > 0 {
			t := s.o.Now().Add(time.Duration(w.TimeoutSeconds) * time.Second)
			request.Timeout = &t
		}
		if w.AfterSequence != "" {
			n, err := strconv.ParseUint(w.AfterSequence, 10, 64)
			if err != nil {
				return 0, nil, fail(400, "malformed", "after_sequence must be a decimal string")
			}
			request.AfterSequence = n
		}
		call.Wait = &request
	case extension.DeclareOutcome:
		var o struct {
			Kind    string     `json:"kind"`
			Summary string     `json:"summary"`
			Data    *payloadIn `json:"data"`
		}
		if err := strict(&o); err != nil {
			return 0, nil, err
		}
		call.Outcome = &domain.Outcome{Kind: domain.OutcomeKind(o.Kind), Summary: o.Summary, Data: o.Data.domain()}
	default:
		var p payloadIn
		if err := strict(&p); err != nil {
			return 0, nil, err
		}
		call.Input = p.domain()
	}
	result, err := s.o.Toolbelt.CallRequest(c.ctx, c.who.token, in.RequestID, call)
	if err != nil {
		return 0, nil, err
	}
	out := map[string]any{}
	if result.Event != nil {
		var scope domain.ID
		_ = s.read(c.ctx, func(tx storage.Tx) error { var e error; scope, e = runtime.EventScopeInTx(tx, *result.Event); return e })
		out["event"] = toEvent(*result.Event, scope)
	}
	if result.Work != nil {
		out["work"] = toWork(*result.Work)
	}
	if result.Wait != nil {
		var due time.Time
		_ = s.read(c.ctx, func(tx storage.Tx) error { due = waitDue(tx, *result.Wait); return nil })
		out["wait"] = toWait(*result.Wait, due)
	}
	if in.Operation == extension.ReadContext {
		out["context"] = payload(result.Context)
	}
	if p := payload(result.Output); p != nil {
		out["output"] = p
	}
	return 200, out, nil
}
