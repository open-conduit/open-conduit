package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
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
	if s.o.Components != nil {
		for k, v := range s.o.Components(c.ctx) {
			components[k] = v
		}
	}
	return 200, map[string]any{"state": HealthState(s.phase(), components), "version": s.o.Version, "instance_id": s.o.InstanceID, "config_generation": u64(generation), "components": components}, nil
}

func (s *Server) phase() string {
	if s.o.Phase == nil {
		return PhaseRunning
	}
	return s.o.Phase()
}

// The daemon's phases (Options.Phase): standby while another instance holds
// the state, starting until its host loop has completed a pass, running, and
// stopping once its shutdown has begun.
const (
	PhaseStandby  = "standby"
	PhaseStarting = "starting"
	PhaseRunning  = "running"
	PhaseStopping = "stopping"
)

// HealthState is GET /health's state: the phase while the daemon is a
// standby, starting or stopping, and otherwise ready when every component is ready
// and degraded when any is not.
func HealthState(phase string, components map[string]string) string {
	if phase == PhaseStandby || phase == PhaseStarting || phase == PhaseStopping {
		return phase
	}
	for _, v := range components {
		if v != "ready" {
			return "degraded"
		}
	}
	return "ready"
}

// Pool is one capacity pool's slots: the limit from settings (zero:
// unbounded), the attempts holding a slot and the ready Work whose last
// start this pool's limit held back.
type Pool struct {
	Name    string `json:"name"`
	Setting string `json:"setting,omitempty"`
	Limit   int    `json:"limit"`
	// Ceiling is the daemon policy's ceiling on this pool, when it sets one.
	Ceiling int `json:"ceiling,omitempty"`
	InUse   int `json:"in_use"`
	Waiting int `json:"waiting"`
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

// Quiescence is GET /quiescence: whether the daemon has anything in flight
// and, if not, when it next has something to do. Until is absent when nothing
// is due. A process-bound duty owes nothing while the daemon is stopped. Every
// package task is listed; one that handed its waiting back carries its next
// due time, which is also an obligation.
type Quiescence struct {
	Quiescent    bool                       `json:"quiescent"`
	Until        *time.Time                 `json:"until,omitempty"`
	Next         string                     `json:"next,omitempty"`
	Reasons      []runtime.QuiescenceReason `json:"reasons"`
	Obligations  []QuiescenceObligation     `json:"obligations"`
	ProcessBound []QuiescenceDuty           `json:"process_bound"`
	PackageTasks []QuiescenceTask           `json:"package_tasks"`
	Startup      *QuiescenceStartup         `json:"startup,omitempty"`
}

// QuiescenceObligation is something the daemon has to do: an awake daemon
// does it at Due, and a stopped one must be started by Latest.
type QuiescenceObligation struct {
	Duty   string    `json:"duty"`
	Due    time.Time `json:"due"`
	Latest time.Time `json:"latest"`
}

// QuiescenceDuty is a periodic duty that needs no process while stopped.
type QuiescenceDuty struct {
	Duty string `json:"duty"`
	Why  string `json:"why"`
}

// QuiescenceTask is a package's background task and its pace. Sleeping says
// it handed its waiting back, and NextDue is the latest it will next run.
type QuiescenceTask struct {
	Task         string     `json:"task"`
	EverySeconds int64      `json:"every_seconds"`
	Sleeping     bool       `json:"sleeping,omitempty"`
	NextDue      *time.Time `json:"next_due,omitempty"`
}

// QuiescenceStartup is what the latest start cost: from the process starting
// to the management socket listening, and to its first completed pass.
type QuiescenceStartup struct {
	At          time.Time `json:"at"`
	ListeningMS int64     `json:"listening_ms"`
	FirstPassMS int64     `json:"first_pass_ms"`
}

func (s *Server) quiescence(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	if s.o.Quiescence == nil {
		return 0, nil, fail(503, "unavailable", "this server doesn't report quiescence")
	}
	q, err := s.o.Quiescence(c.ctx)
	if err != nil {
		return 0, nil, err
	}
	return 200, q, nil
}

// Portability is GET /portability: everything tied to this machine, for an
// operator about to move it. Portable is true when the daemon is drained and
// nothing but Directories, which say what to carry, is listed. Secret references are named by where they are and their
// scheme, never their text, which for cmd: can hold anything.
type Portability struct {
	Portable    bool                `json:"portable"`
	Drain       runtime.DrainStatus `json:"drain"`
	Secrets     []PortableSecret    `json:"secrets"`
	Paths       []PortablePath      `json:"paths"`
	Workspaces  []PortableWorkspace `json:"workspaces"`
	Packages    []PortablePackage   `json:"packages"`
	Directories []PortableDirectory `json:"directories"`
	Unchecked   []string            `json:"unchecked,omitempty"`
}

// PortableSecret is a secret reference whose value won't resolve elsewhere.
type PortableSecret struct {
	Path   string `json:"path"`
	Scheme string `json:"scheme"`
}

// PortablePath is an absolute path in the active config generation.
type PortablePath struct {
	Path  string `json:"path"`
	Value string `json:"value"`
}

// PortableWorkspace is a workspace holding work that exists only there.
type PortableWorkspace struct {
	Provider string `json:"provider"`
	Ref      string `json:"ref"`
	Mode     string `json:"mode,omitempty"`
	Path     string `json:"path,omitempty"`
	Site     string `json:"site,omitempty"`
	Kept     string `json:"kept"`
}

// PortablePackage is a package installed beside the config file.
type PortablePackage struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// PortableDirectory is a directory in the state directory a bundle leaves
// out, and what is lost when it doesn't travel.
type PortableDirectory struct {
	Name string `json:"name"`
	Why  string `json:"why"`
}

func (s *Server) portability(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	if s.o.Portability == nil {
		return 0, nil, fail(503, "unavailable", "this server doesn't report portability")
	}
	out, err := s.o.Portability(c.ctx)
	if err != nil {
		return 0, nil, err
	}
	return 200, out, nil
}

// DrainControl is the daemon's drain, as the management API serves it.
type DrainControl interface {
	DrainStatus(context.Context) (runtime.DrainStatus, error)
	BeginDrain(ctx context.Context, reason string) (runtime.DrainStatus, error)
	EndDrain(context.Context) (runtime.DrainStatus, error)
}

func (s *Server) drainControl() (DrainControl, error) {
	if s.o.Drain == nil {
		return nil, fail(503, "unavailable", "this server doesn't support draining")
	}
	return s.o.Drain, nil
}

func (s *Server) drainStatus(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	d, err := s.drainControl()
	if err != nil {
		return 0, nil, err
	}
	out, err := d.DrainStatus(c.ctx)
	if err != nil {
		return 0, nil, err
	}
	return 200, out, nil
}

func (s *Server) drainBegin(c *call) (int, any, error) {
	var in struct {
		Reason string `json:"reason"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	d, err := s.drainControl()
	if err != nil {
		return 0, nil, err
	}
	if in.Reason != "" {
		c.note("reason", in.Reason)
	}
	out, err := d.BeginDrain(c.ctx, in.Reason)
	if err != nil {
		return 0, nil, err
	}
	return 200, out, nil
}

func (s *Server) drainEnd(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	d, err := s.drainControl()
	if err != nil {
		return 0, nil, err
	}
	out, err := d.EndDrain(c.ctx)
	if err != nil {
		return 0, nil, err
	}
	if s.o.Nudge != nil {
		s.o.Nudge()
	}
	return 200, out, nil
}

func (s *Server) schema(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"api_version": "1", "contract_version": strconv.Itoa(int(extension.ContractVersion)), "operations": s.o.Toolbelt.Operations(), "event_types": s.o.Toolbelt.EventTypes()}, nil
}

// ---- scopes ----

func (s *Server) listScopes(c *call) (int, any, error) {
	q, err := s.query(c, "scopes", listFilters["scopes"]...)
	if err != nil {
		return 0, nil, err
	}
	if e, ok := q.filters["enabled"]; ok && e != "true" && e != "false" {
		return 0, nil, fail(400, "invalid_filter", "enabled must be true or false")
	}
	var items []keyed
	err = s.read(c.ctx, func(tx storage.Tx) error {
		items = nil
		scopes, e := tx.ListScopes()
		for _, sc := range scopes {
			if !c.sees(sc.ID) {
				continue
			}
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

func match(q listQuery, name, value string) bool {
	want, ok := q.filters[name]
	return !ok || want == value
}

// matchAny is match for a filter that takes a comma-separated set, such
// as state=ready,running,waiting.
func matchAny(q listQuery, name, value string) bool {
	want, ok := q.filters[name]
	return !ok || slices.Contains(strings.Split(want, ","), value)
}
func (s *Server) getWork(c *call) (int, any, error) {
	extensions, err := annotationsQuery(c)
	if err != nil {
		return 0, nil, err
	}
	var w domain.Work
	if err := s.read(c.ctx, func(tx storage.Tx) error { var e error; w, e = tx.Work(domain.ID(c.params[0])); return e }); err != nil {
		return 0, nil, err
	}
	if etag(c, w.Revision) {
		return 0, nil, nil
	}
	d, err := s.detail(c.ctx, w, extensions)
	if err != nil {
		return 0, nil, err
	}
	return 200, d, nil
}

// annotationsQuery parses GET /work/{id}'s one query parameter:
// annotations=runtime leaves out the extensions' annotations, keeping the
// host's own, for a view that needs only the hold, the queued input and
// the runtime's notes. Without it every annotation is included.
func annotationsQuery(c *call) (extensions bool, err error) {
	q := c.r.URL.Query()
	if c.r.URL.RawQuery == "" {
		return true, nil
	}
	if len(q) != 1 || len(q["annotations"]) != 1 || q.Get("annotations") != "runtime" {
		return false, fail(400, "invalid_filter", "this endpoint accepts only annotations=runtime")
	}
	return false, nil
}

// ---- events ----

func (s *Server) listEvents(c *call) (int, any, error) {
	q, err := s.query(c, "events", listFilters["events"]...)
	if err != nil {
		return 0, nil, err
	}
	ask, err := with(q, "route")
	if err != nil {
		return 0, nil, err
	}
	var after uint64
	if v, ok := q.filters["after_sequence"]; ok {
		if after, err = strconv.ParseUint(v, 10, 64); err != nil {
			return 0, nil, fail(400, "invalid_filter", "after_sequence must be a decimal string")
		}
	}
	var out []any
	var next string
	start := after
	err = s.read(c.ctx, func(tx storage.Tx) error {
		out, next, after = []any{}, "", start
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
		// A Work's events, and a scope's (with those whose scope isn't
		// recorded yet), are read through their index, not the log.
		page := func(after uint64) ([]domain.AcceptedEvent, error) { return s.o.History.Events(c.ctx, tx, after, 200) }
		if work := q.filters["work_id"]; work != "" {
			page = func(after uint64) ([]domain.AcceptedEvent, error) {
				return s.o.History.WorkEvents(c.ctx, tx, domain.ID(work), after, 200)
			}
		} else if typ := q.filters["type"]; typ != "" {
			// A type's events are read through its index, so following
			// one (the usage feed) costs the same however long the log.
			var within []domain.ID
			if scope := q.filters["scope_id"]; scope != "" {
				within = []domain.ID{domain.ID(scope), ""}
			} else if c.reach != nil {
				within = append(c.scopes(), "")
			}
			page = func(after uint64) ([]domain.AcceptedEvent, error) {
				return s.o.History.TypeEvents(c.ctx, tx, typ, within, after, 200)
			}
		} else if scope := q.filters["scope_id"]; scope != "" {
			page = func(after uint64) ([]domain.AcceptedEvent, error) {
				return s.o.History.ScopeEvents(c.ctx, tx, []domain.ID{domain.ID(scope), ""}, after, 200)
			}
		} else if c.reach != nil {
			// Only the scopes the operator's grants cover.
			within := append(c.scopes(), "")
			page = func(after uint64) ([]domain.AcceptedEvent, error) {
				return s.o.History.ScopeEvents(c.ctx, tx, within, after, 200)
			}
		}
		for {
			batch, e := page(after)
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
				if !c.sees(scope) || !match(q, "scope_id", string(scope)) || !match(q, "type", a.Event.Type) || !(match(q, "work_id", string(a.Event.WorkID)) || match(q, "work_id", string(a.RoutedWorkID))) {
					continue
				}
				if len(out) == q.limit {
					next = s.sign(cursor{Endpoint: "events", Filters: q.norm, Bound: u64(bound), Last: u64(a.Sequence - 1), Expires: s.o.Now().Add(15 * time.Minute).Unix()})
					return nil
				}
				if !ask["route"] {
					out = append(out, toEvent(a, scope))
					continue
				}
				r, e := routeOf(tx, a, scope)
				if e != nil {
					return e
				}
				out = append(out, wireEventWith{wireEvent: toEvent(a, scope), Route: &r})
			}
		}
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"items": out, "next_cursor": next}, nil
}

// getEvent returns one accepted Event; with=route adds the route that
// GET /events/{id}/route returns for it, as the list form does.
func (s *Server) getEvent(c *call) (int, any, error) {
	ask, err := withOnly(c, "route")
	if err != nil {
		return 0, nil, err
	}
	var out any
	err = s.read(c.ctx, func(tx storage.Tx) error {
		a, e := s.o.History.Event(c.ctx, tx, domain.ID(c.params[0]))
		if e != nil {
			return e
		}
		scope, e := runtime.EventScopeInTx(tx, a)
		if e != nil {
			return e
		}
		if !ask["route"] {
			out = toEvent(a, scope)
			return nil
		}
		r, e := routeOf(tx, a, scope)
		out = wireEventWith{wireEvent: toEvent(a, scope), Route: &r}
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
		out, e = routeOf(tx, a, scope)
		return e
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, out, nil
}

// traceRoute is a read-only dry run: how a hypothetical Event would route
// in the scope now. Nothing is accepted, routed or recorded, so it takes no
// Idempotency-Key.
func (s *Server) traceRoute(c *call) (int, any, error) {
	var in struct {
		Type           string     `json:"type"`
		Source         string     `json:"source"`
		CorrelationKey *keyIn     `json:"correlation_key"`
		WorkID         string     `json:"work_id"`
		Data           *payloadIn `json:"data"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	if in.Type == "" || len(in.Type) > 256 {
		return 0, nil, fail(400, "malformed", "type is required")
	}
	if in.CorrelationKey != nil && (in.CorrelationKey.Source == "" || in.CorrelationKey.ExternalID == "") {
		return 0, nil, fail(400, "malformed", "correlation_key needs source and external_id")
	}
	source := in.Source
	if source == "" {
		source = runtime.ManualSource
	}
	now := s.o.Now()
	e := domain.Event{ID: "trace", Type: in.Type, Source: source, DeliveryID: "trace", WorkID: domain.ID(in.WorkID), CorrelationKey: in.CorrelationKey.domain(), Data: in.Data.domain(), OccurredAt: now, ReceivedAt: now}
	// A trace refuses what acceptance would refuse, so it never explains
	// an Event that could not happen.
	err := runtime.CheckTraceEvent(e)
	if err == nil && s.o.AdmitTrace != nil {
		err = s.o.AdmitTrace(domain.ID(c.params[0]), e)
	}
	if err != nil {
		return 0, nil, fail(422, "invalid_request", "acceptance would refuse this event: "+err.Error())
	}
	t, err := s.o.Control.TraceRoute(c.ctx, domain.ID(c.params[0]), e)
	if err != nil {
		return 0, nil, err
	}
	return 200, toRouteTrace(t), nil
}

// routeSamples lists the sample Events the scope's route handlers offer
// for a trace ("Suppose a new issue is opened").
func (s *Server) routeSamples(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	samples, err := s.o.Control.RouteSamples(c.ctx, domain.ID(c.params[0]))
	if err != nil {
		return 0, nil, err
	}
	out := []wireRouteSample{}
	for _, x := range samples {
		w := wireRouteSample{ID: x.Handler + "/" + x.ID, Title: x.Title, Route: x.Route, Handler: x.Handler, Type: x.Event.Type, Source: x.Event.Source, WorkID: string(x.Event.WorkID)}
		if k := x.Event.CorrelationKey; k != nil {
			w.CorrelationKey = &wireKey{Source: k.Source, ExternalID: k.ExternalID}
		}
		if x.Event.Data.Schema != "" || len(x.Event.Data.Data) > 0 {
			w.Data = &wirePayload{Schema: x.Event.Data.Schema, Data: x.Event.Data.Data}
		}
		out = append(out, w)
	}
	return 200, map[string]any{"items": out}, nil
}

// routeOf is a's routing explanation, for GET /events/{id}/route and
// GET /events?with=route alike.
func routeOf(tx storage.Tx, a domain.AcceptedEvent, scope domain.ID) (wireEventRoute, error) {
	rec, ok, err := runtime.RouteDecisionInTx(tx, a.Event.ID)
	if err != nil {
		return wireEventRoute{}, err
	}
	out := wireEventRoute{EventID: string(a.Event.ID), Type: a.Event.Type, ScopeID: string(scope), Routed: a.Routed, Disposition: a.Disposition, RoutedWorkID: string(a.RoutedWorkID)}
	if ok {
		out.Route, out.Binding, out.Generation, out.Policy, out.Reason = rec.Route, rec.Binding, rec.Generation, rec.Policy, rec.Reason
		out.DisplayRefused = rec.DisplayRefused
		out.Error, out.Attempts = rec.Error, rec.Attempts
		for _, f := range rec.Refusals {
			out.Refusals = append(out.Refusals, wireRouteRefusal{Route: f.Route, Reason: f.Reason})
		}
		if out.RoutedWorkID == "" {
			out.RoutedWorkID = string(rec.WorkID)
		}
	}
	if !a.Routed {
		f, failing, err := runtime.RouteFailureInTx(tx, a.Event.ID)
		if err != nil {
			return wireEventRoute{}, err
		}
		if failing {
			out.Route, out.Reason, out.Error, out.Attempts = f.Route, f.Code, f.Error, f.Attempts
		}
	}
	return out, nil
}

// ---- executions ----

func (s *Server) listExecutions(c *call) (int, any, error) {
	q, err := s.query(c, "executions", listFilters["executions"]...)
	if err != nil {
		return 0, nil, err
	}
	var items []keyed
	err = s.read(c.ctx, func(tx storage.Tx) error {
		items = nil
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
			if !c.sees(scope) || !match(q, "scope_id", string(scope)) || !match(q, "state", string(x.State)) || !match(q, "work_id", string(x.WorkID)) {
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
	q, err := s.query(c, "waits", listFilters["waits"]...)
	if err != nil {
		return 0, nil, err
	}
	var out map[string]any
	err = s.read(c.ctx, func(tx storage.Tx) error {
		// Active Waits are read through their index, not every Wait ever
		// registered.
		var waits []domain.Wait
		var e error
		if q.filters["state"] == string(domain.WaitActive) && q.filters["work_id"] == "" {
			waits, e = tx.ActiveWaits()
		} else {
			waits, e = tx.ListWaits(domain.ID(q.filters["work_id"]))
		}
		if e != nil {
			return e
		}
		var items []lazyKeyed
		for _, wait := range waits {
			if _, ok := q.filters["scope_id"]; ok || c.reach != nil {
				w, e := tx.Work(wait.WorkID)
				if e != nil {
					return e
				}
				if !c.sees(w.ScopeID) || !match(q, "scope_id", string(w.ScopeID)) {
					continue
				}
			}
			if !match(q, "execution_id", string(wait.ExecutionID)) || !match(q, "state", string(wait.State)) || !match(q, "work_id", string(wait.WorkID)) {
				continue
			}
			// Its due time reads its timer: only for the page.
			items = append(items, lazyKeyed{key: createdKey(wait.CreatedAt, wait.ID), build: func() (any, bool, error) { return toWait(wait, waitDue(tx, wait)), true, nil }})
		}
		out, e = s.lazyPage("waits", q, s.nowBound(), items)
		return e
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, out, nil
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

// artifacts lists every artifact of the executions keep accepts, each
// attempt's earlier turns' first.
func (s *Server) artifacts(c *call, keep func(domain.Execution) bool) ([]wireArtifact, []domain.Execution, error) {
	var out []wireArtifact
	var xs []domain.Execution
	err := s.read(c.ctx, func(tx storage.Tx) error {
		out = nil
		var e error
		xs, e = tx.ListExecutions("")
		if e != nil {
			return e
		}
		scopes := map[domain.ID]domain.ID{}
		for _, x := range xs {
			if len(x.AllArtifacts()) == 0 || !keep(x) {
				continue
			}
			// A list holds only the runs of scopes the operator's grants
			// cover.
			if c.reach != nil {
				scope, ok := scopes[x.WorkID]
				if !ok {
					w, e := tx.Work(x.WorkID)
					if e != nil {
						return e
					}
					scope, scopes[x.WorkID] = w.ScopeID, w.ScopeID
				}
				if !c.sees(scope) {
					continue
				}
			}
			refs, e := runtime.ArtifactsInTx(tx, x)
			if e != nil {
				return e
			}
			for _, a := range refs {
				out = append(out, wireArtifact{ID: a.ID, Size: a.Size, MediaType: a.MediaType, SHA256: a.SHA256, WorkID: string(x.WorkID), ExecutionID: string(x.ID), Segment: a.Segment})
			}
		}
		return nil
	})
	return out, xs, err
}
func (s *Server) listArtifacts(c *call) (int, any, error) {
	q, err := s.query(c, "artifacts", listFilters["artifacts"]...)
	if err != nil {
		return 0, nil, err
	}
	// Only the runs the filters keep have their attempt state read.
	all, _, err := s.artifacts(c, func(x domain.Execution) bool {
		return match(q, "work_id", string(x.WorkID)) && match(q, "execution_id", string(x.ID))
	})
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
	all, _, err := s.artifacts(c, func(x domain.Execution) bool {
		for _, a := range x.AllArtifacts() {
			if a.ID == c.params[0] {
				return true
			}
		}
		return false
	})
	if err != nil {
		return 0, nil, err
	}
	var ref *domain.ArtifactRef
	owner := ""
	for _, a := range all {
		if a.ID == c.params[0] {
			ref = &domain.ArtifactRef{ID: a.ID, Size: a.Size, MediaType: a.MediaType, SHA256: a.SHA256}
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

// eventReason reads an Event control's {reason}.
func eventReason(c *call) (string, error) {
	var in struct {
		Reason string `json:"reason"`
	}
	if err := c.decode(&in); err != nil {
		return "", err
	}
	if len(in.Reason) > 1024 {
		return "", fail(400, "malformed", "reason is too long")
	}
	return in.Reason, nil
}

// skipEvent routes an unrouted Event as skipped (issue #342).
func (s *Server) skipEvent(c *call) (int, any, error) {
	reason, err := eventReason(c)
	if err != nil {
		return 0, nil, err
	}
	event, err := s.o.Control.SkipEvent(c.ctx, domain.ID(c.params[0]), reason, c.key)
	if err != nil {
		return 0, nil, err
	}
	return 202, map[string]any{"event_id": string(event), "routed_event_id": c.params[0]}, nil
}

// rerouteEvent routes an Event again: an unrouted one at once, one routing
// gave up on or skipped as an accepted copy.
func (s *Server) rerouteEvent(c *call) (int, any, error) {
	reason, err := eventReason(c)
	if err != nil {
		return 0, nil, err
	}
	event, routed, err := s.o.Control.RerouteEvent(c.ctx, domain.ID(c.params[0]), reason, c.key)
	if err != nil {
		return 0, nil, err
	}
	return 202, map[string]any{"event_id": string(event), "routed_event_id": string(routed)}, nil
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

// config is the active generation's wiring: each scope's authorization
// ceiling, extensions, routes and bindings by name and type, and the
// connections they use, with what an extension or route handler says its
// config sets up (a schedule's jobs) and a summary of each extension's,
// executor's and resource's config. It carries no opaque config and no
// secret reference text.
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
		a := e.Authorization
		sc := wireConfigScope{ID: string(e.ID), Name: e.Name, ParentID: string(e.ParentID), Enabled: e.Enabled, Source: e.Source,
			Authorization: wireAuthorization{Connections: nonNil(a.Connections), Operations: nonNil(a.Operations), ReadScopes: nonNil(a.ReadScopes), FileScopes: a.FileScopes},
			Extensions:    []wireConfigExtension{}, Routes: []wireConfigRoute{}, Bindings: []wireConfigBinding{}}
		for _, x := range e.Extensions {
			t := reg.Extensions[x.Type]
			wx := wireConfigExtension{Name: x.Name, Type: x.Type, Connection: x.Connection, Ingress: t.Ingress, Settings: settings(x.Config, t.Settings)}
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
			r := b.Requires
			wb := wireConfigBinding{Name: b.Name, Executor: b.Executor, Connection: b.Connection, Requires: wireRequires{r.Interrupt, r.Inject, r.Resume, r.Workspace, r.Toolbelt}, Operations: nonNil(b.Operations), Settings: settings(b.Config, executorSettings(reg, b.Executor))}
			if b.Resource != nil {
				wb.Resource = b.Resource.Provider
				wb.ResourceSettings = settings(b.Resource.Config, reg.ResourceSettings[b.Resource.Provider])
			}
			for _, g := range b.Grants {
				wb.Grants = append(wb.Grants, wireGrant{Name: g.Name, Label: g.Label, Operations: nonNil(g.Operations)})
			}
			sc.Bindings = append(sc.Bindings, wb)
		}
		out.Scopes = append(out.Scopes, sc)
	}
	return 200, out, nil
}

// executorSettings is the executor registration's settings allowlist, if
// it declares one.
func executorSettings(reg config.Registry, executor string) func(json.RawMessage) []extension.Setting {
	if d, ok := reg.Executors[executor].(extension.SettingsDescriber); ok {
		return d.Settings
	}
	return nil
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
	return 200, map[string]any{"valid": true, "candidate_hash": snapshot.FileHash, "config": snapshot.Redacted()}, nil
}

// applyConfig applies config, or config_text: the file's text as a JSON
// string, validated as parsed and saved byte for byte, so the operator's
// layout and key order survive.
// exportConfig is the active generation as config file text, with secret
// references as reference text (never values): what config apply would take
// back, and the path of the config file the daemon saves to. It is for an operator whose config file is gone or out of step.
func (s *Server) exportConfig(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	snap, generation := s.o.Config.Current()
	if snap == nil {
		return 0, nil, fail(409, "state_conflict", "no configuration generation is active")
	}
	text, err := config.ExportFile(snap.Normalized)
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"generation": u64(generation), "hash": snap.Hash, "path": s.o.Config.Path(), "config_text": string(text)}, nil
}

func (s *Server) applyConfig(c *call) (int, any, error) {
	var in struct {
		Config             json.RawMessage `json:"config"`
		ConfigText         *string         `json:"config_text"`
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
	data := []byte(in.Config)
	if in.ConfigText != nil {
		if len(in.Config) > 0 {
			return 0, nil, fail(400, "malformed", "give config or config_text, not both")
		}
		data = []byte(*in.ConfigText)
	}
	generation, hash, err := s.o.Config.Apply(c.ctx, data, in.CandidateHash, expected, s.configRecorder(c))
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"generation": u64(generation), "hash": hash}, nil
}

// getInclude is a submitted include's fragment as its team edits it: the
// scope it sits under, the text POST /config/includes/{name}/apply takes
// back, and the hash that apply names as the one it replaces.
func (s *Server) getInclude(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	f, generation, err := s.o.Config.Fragment(c.params[0])
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"name": f.Name, "parent": f.Parent, "generation": u64(generation), "hash": f.Hash, "fragment_text": string(f.Text)}, nil
}

// applyInclude applies a submitted include's fragment, given as fragment
// (JSON) or fragment_text (its text as a JSON string), over expected_hash,
// the fragment it replaces. It is recorded in the operator trail under the
// caller.
func (s *Server) applyInclude(c *call) (status int, data any, err error) {
	name := c.params[0]
	defer func() { s.observeOperator(c, "operator.include_applied", name, "", err) }()
	var in struct {
		Fragment     json.RawMessage `json:"fragment"`
		FragmentText *string         `json:"fragment_text"`
		ExpectedHash string          `json:"expected_hash"`
	}
	if err = c.decode(&in); err != nil {
		return 0, nil, err
	}
	if in.ExpectedHash == "" {
		return 0, nil, fail(400, "malformed", "expected_hash is required: the hash GET /v1/config/includes/{name} gives")
	}
	fragment := []byte(in.Fragment)
	if in.FragmentText != nil {
		if len(in.Fragment) > 0 {
			return 0, nil, fail(400, "malformed", "give fragment or fragment_text, not both")
		}
		fragment = []byte(*in.FragmentText)
	}
	generation, hash, err := s.o.Config.ApplyFragment(c.ctx, name, fragment, in.ExpectedHash, func(parent string) bool { return c.sees(domain.ID(parent)) }, s.configRecorder(c))
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"generation": u64(generation), "hash": hash}, nil
}

// ConfigCandidate is a configuration a Work proposes: the changes it is
// made of, for the operator to review, and once one is approved, ready for
// POST /config/apply: its file text (which may carry reference text), the
// generation it is based on and its hash.
type ConfigCandidate struct {
	Text           []byte
	BaseGeneration string
	Hash           string
	Changes        []extension.CandidateChange
	// ApproveAll and Validate are the owning extension's action IDs.
	ApproveAll, Validate string
}

type wireChange struct {
	ID       string          `json:"id"`
	Op       string          `json:"op"`
	Pointer  string          `json:"pointer"`
	State    string          `json:"state"`
	Why      string          `json:"why,omitempty"`
	Before   json.RawMessage `json:"before,omitempty"`
	After    json.RawMessage `json:"after,omitempty"`
	Problems []string        `json:"problems,omitempty"`
	Approve  string          `json:"approve,omitempty"`
	Reject   string          `json:"reject,omitempty"`
}

// configCandidate is what a Work proposes for the operator (a setup
// session's change set): every change with its state, before, after and
// the action IDs that decide it, and, once a change is approved, the file
// text, the generation it is based on and its hash, ready for POST
// /config/apply. It is an operator-class read: the text may carry
// reference text.
func (s *Server) configCandidate(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	if s.o.ConfigCandidate == nil {
		return 0, nil, storage.ErrNotFound
	}
	var w domain.Work
	if err := s.o.Store.Transact(c.ctx, storage.Guard{Now: s.o.Now()}, func(tx storage.Tx) error {
		var err error
		w, err = tx.Work(domain.ID(c.params[0]))
		return err
	}); err != nil {
		return 0, nil, err
	}
	cand, err := s.o.ConfigCandidate(c.ctx, w)
	if err != nil {
		return 0, nil, err
	}
	changes := make([]wireChange, 0, len(cand.Changes))
	for _, x := range cand.Changes {
		changes = append(changes, wireChange(x))
	}
	c.w.Header().Set("Cache-Control", "no-store")
	out := map[string]any{"base_generation": cand.BaseGeneration, "changes": changes}
	if cand.Hash != "" {
		out["config_text"], out["candidate_hash"] = string(cand.Text), cand.Hash
	}
	if cand.ApproveAll != "" {
		out["approve_all"] = cand.ApproveAll
	}
	if cand.Validate != "" {
		out["validate"] = cand.Validate
	}
	return 200, out, nil
}

// Page is a page an extension contributes to the web view.
type Page struct {
	Extension string
	extension.Page
}

// listPages lists the pages extensions contribute (extension.Page), for
// the web view's navigation and its generic page.
func (s *Server) listPages(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	type wirePage struct {
		Extension string   `json:"extension"`
		ID        string   `json:"id"`
		Title     string   `json:"title"`
		Summary   string   `json:"summary,omitempty"`
		Entry     string   `json:"entry"`
		WorkKind  string   `json:"work_kind"`
		Start     string   `json:"start"`
		Compose   []string `json:"compose,omitempty"`
		Route     string   `json:"route,omitempty"`
	}
	out := []wirePage{}
	if s.o.Pages != nil {
		for _, p := range s.o.Pages() {
			out = append(out, wirePage{Extension: p.Extension, ID: p.ID, Title: p.Title, Summary: p.Summary, Entry: p.Entry, WorkKind: p.WorkKind, Start: p.Start, Compose: p.Compose, Route: p.Route})
		}
	}
	return 200, map[string]any{"pages": out}, nil
}

// Extension is one installed extension package as GET /extensions lists
// it: its name, display name (the manifest's title, "" for none) and
// version, and whether the daemon is running it. The daemon runs the
// packages its configuration uses; the rest are installed but not started.
type Extension struct {
	Name, Title, Version string
	Running              bool
}

// listExtensions lists the installed extension packages (Options.Extensions)
// by name: the web view's source labels, and which of them are running.
func (s *Server) listExtensions(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	type wireExtension struct {
		Name    string `json:"name"`
		Title   string `json:"title"`
		Version string `json:"version,omitempty"`
		Running bool   `json:"running"`
	}
	out := []wireExtension{}
	if s.o.Extensions != nil {
		for _, x := range s.o.Extensions() {
			out = append(out, wireExtension{Name: x.Name, Title: x.Title, Version: x.Version, Running: x.Running})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return 200, map[string]any{"extensions": out}, nil
}

// rotate never stores the replacement in idempotency history; a lost response
// recovers through the locked local CLI (RecoverOperator). The replacement
// belongs to the same operator.
func (s *Server) rotate(c *call) (int, any, error) {
	var in struct {
		ExpectedTokenID string `json:"expected_token_id"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	if c.who.issuer != "" {
		return 0, nil, fail(409, "not_rotatable", "this credential was issued by "+c.who.issuer+"; get a new one from it")
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
		if !r.active(s.o.Now()) {
			return fail(401, "unauthenticated", "credential revoked or expired")
		}
		r.Revoked, r.RevokedAt = true, s.o.Now()
		b, _ = json.Marshal(r)
		if err = tx.Put(authNamespace, "operator/"+digest, b); err != nil {
			return err
		}
		// The replacement keeps the caller's expiry: rotating never
		// extends how long a credential lasts.
		if file, err = issueOperatorInTx(tx, s.o.Now(), r.Name, r.Role, r.Grants, r.ActFor, r.ExpiresAt, r.Host); err != nil {
			return err
		}
		c.target = "credential/" + r.TokenID
		c.note("replacement", file.TokenID)
		return s.auditInTx(tx, c)
	})
	if err != nil {
		return 0, nil, err
	}
	c.w.Header().Set("Cache-Control", "no-store")
	return 200, map[string]any{"token_id": file.TokenID, "token": file.Token}, nil
}

// revoke revokes an operator credential (otok_) or an execution token. An
// operator may revoke its own credential or a named operator's; the owner's
// own credential is revoked only by itself, or by auth recover, so no named
// operator can lock the owner out.
func (s *Server) revoke(c *call) (int, any, error) {
	var in struct {
		TokenID string `json:"token_id"`
		Reason  string `json:"reason"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	c.target = "credential/" + in.TokenID
	if in.Reason != "" && len(in.Reason) <= 500 {
		c.note("reason", in.Reason)
	}
	if strings.HasPrefix(in.TokenID, "otok_") {
		var at time.Time
		err := s.o.Store.Transact(c.ctx, storage.Guard{Now: s.o.Now()}, func(tx storage.Tx) error {
			var digest string
			if in.TokenID == c.who.tokenID {
				digest = runtime.TokenDigest(c.who.token)
			} else {
				b, err := tx.Get(authNamespace, "operator_id/"+in.TokenID)
				if err != nil {
					return err
				}
				if err = json.Unmarshal(b, &digest); err != nil {
					return err
				}
			}
			b, err := tx.Get(authNamespace, "operator/"+digest)
			if err != nil {
				return err
			}
			var r operatorRecord
			if err = json.Unmarshal(b, &r); err != nil {
				return err
			}
			if r.TokenID != in.TokenID || in.TokenID != c.who.tokenID && r.Name == "" {
				return storage.ErrNotFound
			}
			// Revoking someone else's credential is managing operators.
			if in.TokenID != c.who.tokenID && !c.who.may(RoleAdmin) {
				return forbiddenRole(c.who, RoleAdmin)
			}
			if !r.Revoked {
				r.Revoked, r.RevokedAt = true, s.o.Now()
			}
			at = r.RevokedAt
			b, _ = json.Marshal(r)
			if err = tx.Put(authNamespace, "operator/"+digest, b); err != nil {
				return err
			}
			c.note("operator", extension.CallerOperator+":"+r.name())
			return s.auditInTx(tx, c)
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

type wireOperator struct {
	Name string `json:"name"`
	// Role is the daemon-wide role, "" for an operator that holds only
	// grants.
	Role   string  `json:"role"`
	Grants []Grant `json:"grants"`
	// ActFor is the kind of subject the credential may act for, "" for
	// none.
	ActFor    string `json:"act_for,omitempty"`
	TokenID   string `json:"token_id"`
	CreatedAt string `json:"created_at"`
	RevokedAt string `json:"revoked_at,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// listOperators is GET /auth/operators: every operator credential, active or
// revoked, by name. It never shows a token.
func (s *Server) listOperators(c *call) (int, any, error) {
	var out []wireOperator
	err := s.read(c.ctx, func(tx storage.Tx) error {
		out = []wireOperator{}
		rows, err := tx.List(authNamespace, "operator/")
		if err != nil {
			return err
		}
		for _, row := range rows {
			var r operatorRecord
			if err = json.Unmarshal(row.Value, &r); err != nil {
				return err
			}
			o := wireOperator{Name: r.name(), Role: r.role(), Grants: readGrants(r.Grants), ActFor: r.ActFor, TokenID: r.TokenID, CreatedAt: ts(r.CreatedAt)}
			if r.Revoked {
				o.RevokedAt = ts(r.RevokedAt)
			}
			if !r.ExpiresAt.IsZero() {
				o.ExpiresAt = ts(r.ExpiresAt)
			}
			out = append(out, o)
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return 200, map[string]any{"operators": out}, nil
}

// addOperator is POST /auth/operators: a credential for a named operator,
// whose every control is then recorded as operator:<name>. Like rotate, the
// token is returned once and never kept in idempotency history. An optional
// ttl (a Go duration such as "8h") makes it expire; without one it lasts
// until it is revoked. A name has at most one active credential that never
// expires, but any number of expiring ones beside it, so a broker can give
// one person a credential per session. Every credential of a name is the
// same operator: its actor, its role checks against what it carries, and
// its Idempotency-Keys, which are the operator's, not the session's.
// grants give the operator view or operate at scopes, each covering the
// scope's subtree; with grants, role (the daemon-wide one) may be left
// out, and the operator then holds nothing beyond its grants. act_for
// lets the credential act for subjects of that one kind (see subject.go).
func (s *Server) addOperator(c *call) (int, any, error) {
	var in struct {
		Name   string  `json:"name"`
		Role   string  `json:"role"`
		Grants []Grant `json:"grants"`
		ActFor string  `json:"act_for"`
		TTL    string  `json:"ttl"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	if in.ActFor != "" {
		if err := s.checkActFor(in.ActFor); err != nil {
			return 0, nil, err
		}
	}
	if !ValidRole(in.Role) && (in.Role != "" || len(in.Grants) == 0) {
		return 0, nil, fail(400, "malformed", "role must be admin, operate or view, or left out when grants are given")
	}
	if !validGrants(in.Grants) {
		return 0, nil, fail(400, "malformed", "grants are up to 64 of {scope, role}, each a scope ID and view or operate, no scope twice; admin is only ever daemon-wide")
	}
	if !ValidOperatorName(in.Name) {
		return 0, nil, fail(400, "malformed", "name must be 1-32 lower case letters, digits or dashes, start with a letter, and not be local")
	}
	var ttl time.Duration
	if in.TTL != "" {
		var err error
		if ttl, err = time.ParseDuration(in.TTL); err != nil || ttl <= 0 {
			return 0, nil, fail(400, "malformed", "ttl must be a positive duration such as 30m or 8h")
		}
	}
	now := s.o.Now()
	var expires time.Time
	if ttl > 0 {
		expires = now.Add(ttl)
	}
	var file TokenFile
	err := s.o.Store.Transact(c.ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		if expires.IsZero() {
			rows, err := tx.List(authNamespace, "operator/")
			if err != nil {
				return err
			}
			for _, row := range rows {
				var r operatorRecord
				if err = json.Unmarshal(row.Value, &r); err != nil {
					return err
				}
				if r.Name == in.Name && r.ExpiresAt.IsZero() && !r.Revoked {
					return fail(409, "operator_exists", "operator "+in.Name+" already has an active credential that never expires; revoke it first, or give this one a ttl")
				}
			}
		}
		var err error
		if file, err = issueOperatorInTx(tx, now, in.Name, in.Role, in.Grants, in.ActFor, expires, ""); err != nil {
			return err
		}
		c.target = "credential/" + file.TokenID
		c.note("operator", extension.CallerOperator+":"+in.Name)
		c.note("role", in.Role)
		c.note("grants", grantsNote(in.Grants))
		c.note("act_for", in.ActFor)
		c.note("expires_at", ts(expires))
		return s.auditInTx(tx, c)
	})
	if err != nil {
		return 0, nil, err
	}
	c.w.Header().Set("Cache-Control", "no-store")
	out := map[string]any{"name": in.Name, "role": in.Role, "grants": readGrants(in.Grants), "token_id": file.TokenID, "token": file.Token}
	if in.ActFor != "" {
		out["act_for"] = in.ActFor
	}
	if !expires.IsZero() {
		out["expires_at"] = ts(expires)
	}
	return 201, out, nil
}

// SweepOperators deletes the record of every operator credential whose
// expiry passed more than grace ago, with its token ID index, up to limit
// at a time, and reports how many it deleted. A deleted credential is
// unknown, which authenticate refuses just as it refused the expired one.
// Credentials that never expire are kept, revoked or not.
func (s *Server) SweepOperators(ctx context.Context, grace time.Duration, limit int) (int, error) {
	deleted := 0
	err := s.o.Store.Transact(ctx, storage.Guard{Now: s.o.Now()}, func(tx storage.Tx) error {
		deleted = 0
		rows, err := tx.List(authNamespace, "operator/")
		if err != nil {
			return err
		}
		cutoff := s.o.Now().Add(-grace)
		for _, row := range rows {
			if deleted >= limit {
				break
			}
			var r operatorRecord
			if err = json.Unmarshal(row.Value, &r); err != nil {
				return err
			}
			if r.ExpiresAt.IsZero() || !r.ExpiresAt.Before(cutoff) {
				continue
			}
			if err = tx.Delete(authNamespace, row.Key); err != nil {
				return err
			}
			if err = tx.Delete(authNamespace, "operator_id/"+r.TokenID); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			deleted++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// ---- toolbelt ----

// toolbeltGuidance answers the agent guidance of the operations the
// caller's token is granted, each one's summary and input (an MCP
// server's instructions) and the name of every operation it is granted. On an execution's own socket the token must
// belong to that execution.
func (s *Server) toolbeltGuidance(c *call) (int, any, error) {
	guidance, err := s.o.Toolbelt.Guidance(c.ctx, c.who.token, scopedExecution(c.ctx))
	if err != nil {
		return 0, nil, err
	}
	if guidance == nil {
		guidance = []string{}
	}
	described, err := s.o.Toolbelt.Described(c.ctx, c.who.token, scopedExecution(c.ctx))
	if err != nil {
		return 0, nil, err
	}
	operations := make([]map[string]any, 0, len(described))
	for _, op := range described {
		o := map[string]any{"name": op.Name}
		if op.Summary != "" {
			o["summary"] = op.Summary
		}
		if len(op.Input) > 0 {
			o["input"] = op.Input
		}
		operations = append(operations, o)
	}
	granted, err := s.o.Toolbelt.Granted(c.ctx, c.who.token, scopedExecution(c.ctx))
	if err != nil {
		return 0, nil, err
	}
	if granted == nil {
		granted = []string{}
	}
	return 200, map[string]any{"guidance": guidance, "operations": operations, "granted": granted}, nil
}

// toolbeltServing answers which of the operations the caller's token is
// granted serve ?capability=. Only the host calls them (its git transport
// asks for git.credential); this says which there are. On an execution's
// own socket the token must belong to that execution.
func (s *Server) toolbeltServing(c *call) (int, any, error) {
	capability := c.r.URL.Query().Get("capability")
	if capability == "" {
		return 0, nil, fail(400, "bad_request", "capability is required")
	}
	ops, err := s.o.Toolbelt.Serving(c.ctx, c.who.token, scopedExecution(c.ctx), capability)
	if err != nil {
		return 0, nil, err
	}
	if ops == nil {
		ops = []string{}
	}
	return 200, map[string]any{"operations": ops}, nil
}

func (s *Server) toolbeltCall(c *call) (int, any, error) {
	var in struct {
		Operation   string          `json:"operation"`
		Input       json.RawMessage `json:"input"`
		RequestID   string          `json:"request_id"`
		ScopeID     string          `json:"scope_id"`
		WorkID      string          `json:"work_id"`
		ExecutionID string          `json:"execution_id"`
		Capability  string          `json:"capability"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	if in.RequestID == "" {
		in.RequestID = c.key
	}
	// A capability is the host's to ask for (its git transport asks for
	// git.credential), never a toolbelt request's: what an operation
	// serving one answers is often a secret, and nothing in the run may be
	// handed it.
	if in.Capability != "" {
		return 0, nil, &extension.OperationError{Code: extension.CodeCapabilityOnly, Message: extension.DescribeCapabilityCall(in.Capability, domain.Payload{Data: in.Input}).Direct}
	}
	call := extension.ToolCall{Operation: in.Operation, ScopeID: domain.ID(in.ScopeID), WorkID: domain.ID(in.WorkID), ExecutionID: domain.ID(in.ExecutionID)}
	// On an execution's own socket the call asserts that execution, so a
	// token issued to any other execution is refused by the toolbelt.
	if id := scopedExecution(c.ctx); id != "" {
		if call.ExecutionID != "" && call.ExecutionID != id {
			return 0, nil, fail(403, "forbidden", "execution authority rejected")
		}
		call.ExecutionID = id
	}
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
	case extension.CancelWork:
		var w struct {
			WorkID string `json:"work_id"`
			Reason string `json:"reason"`
		}
		if err := strict(&w); err != nil {
			return 0, nil, err
		}
		if w.WorkID == "" {
			return 0, nil, fail(400, "malformed", "work_id is required")
		}
		if len(w.Reason) > extension.MaxCancelReason {
			return 0, nil, fail(400, "malformed", "reason is too long")
		}
		call.Cancel = &extension.CancelRequest{Work: domain.ID(w.WorkID), Reason: w.Reason}
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
