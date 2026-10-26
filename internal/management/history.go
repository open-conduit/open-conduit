package management

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// ---- unified run history ----

type wireTrigger struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Source      string `json:"source"`
	OccurredAt  string `json:"occurred_at,omitempty"`
	CausationID string `json:"causation_id,omitempty"`
}
type wireUsage struct {
	InputTokens  *uint64  `json:"input_tokens,omitempty"`
	OutputTokens *uint64  `json:"output_tokens,omitempty"`
	CachedTokens *uint64  `json:"cached_tokens,omitempty"`
	Cost         *float64 `json:"cost,omitempty"`
	Currency     string   `json:"currency,omitempty"`
}
type wireWorkspace struct {
	Kind    string `json:"kind,omitempty"`
	Ref     string `json:"ref"`
	Dir     string `json:"dir,omitempty"`
	Summary string `json:"summary,omitempty"`
}
type wireRetry struct {
	FailureCount  int    `json:"failure_count"`
	EligibleAt    string `json:"eligible_at,omitempty"`
	Blocked       bool   `json:"blocked"`
	Exhausted     bool   `json:"exhausted"`
	BlockedReason string `json:"blocked_reason,omitempty"`
}
type wireRetention struct {
	State           string `json:"state"`
	ExpiredAt       string `json:"expired_at,omitempty"`
	ContentReleased bool   `json:"content_released"`
}

// wireRun is one attempt in the unified history: the execution record plus
// generic trigger, timeout, usage, retry and retention context. The same
// shape serves every executor.
type wireRun struct {
	wireExecution
	WorkKind                string         `json:"work_kind"`
	WorkState               string         `json:"work_state"`
	Trigger                 *wireTrigger   `json:"trigger,omitempty"`
	CurrentTrigger          *wireTrigger   `json:"current_trigger,omitempty"`
	Executor                string         `json:"executor,omitempty"`
	ConfigGeneration        string         `json:"config_generation,omitempty"`
	LaunchState             string         `json:"launch_state,omitempty"`
	ResourceExcluded        bool           `json:"resource_excluded"`
	Deadline                string         `json:"deadline,omitempty"`
	RemainingTimeoutSeconds int64          `json:"remaining_timeout_seconds,omitempty"`
	DurationMS              int64          `json:"duration_ms,omitempty"`
	Usage                   *wireUsage     `json:"usage,omitempty"`
	Workspace               *wireWorkspace `json:"workspace,omitempty"`
	Inputs                  int            `json:"inputs"`
	WaitID                  string         `json:"wait_id,omitempty"`
	WakeEventID             string         `json:"wake_event_id,omitempty"`
	Retry                   *wireRetry     `json:"retry,omitempty"`
	Retention               wireRetention  `json:"retention"`
}

func trigger(e *domain.Event) *wireTrigger {
	if e == nil {
		return nil
	}
	return &wireTrigger{ID: string(e.ID), Type: e.Type, Source: e.Source, OccurredAt: ts(e.OccurredAt), CausationID: string(e.CausationID)}
}

func toRun(r runtime.Run) wireRun {
	x := r.Execution
	out := wireRun{wireExecution: toExecution(x, r.ScopeID), WorkKind: r.WorkKind, WorkState: string(r.WorkState), Trigger: trigger(r.Trigger), CurrentTrigger: trigger(r.CurrentTrigger), Executor: r.Executor, ConfigGeneration: r.Generation, LaunchState: r.LaunchState, ResourceExcluded: r.ResourceExcluded, Inputs: r.Inputs, WaitID: string(r.WaitID), WakeEventID: string(r.WakeEventID), Retention: wireRetention{State: "retained"}}
	if !x.State.Terminal() && x.State != domain.ExecutionParked {
		out.Deadline = ts(r.Deadline)
	}
	if r.RemainingTimeout > 0 {
		out.RemainingTimeoutSeconds = int64(r.RemainingTimeout.Seconds())
	}
	if x.StartedAt != nil && x.FinishedAt != nil {
		out.DurationMS = x.FinishedAt.Sub(*x.StartedAt).Milliseconds()
	}
	if u := r.Usage; u != nil {
		out.Usage = &wireUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedTokens: u.CachedTokens, Cost: u.Cost, Currency: u.Currency}
	}
	if ws := r.Workspace; ws != nil {
		out.Workspace = &wireWorkspace{Kind: ws.Kind, Ref: ws.Ref, Dir: ws.Dir, Summary: ws.Summary}
	}
	if rt := r.Retry; rt != nil {
		out.Retry = &wireRetry{FailureCount: rt.FailureCount, EligibleAt: ts(rt.EligibleAt), Blocked: rt.Blocked, Exhausted: rt.Exhausted, BlockedReason: rt.BlockedReason}
	}
	if r.Retention.Expired {
		out.Retention = wireRetention{State: "expired", ExpiredAt: ts(r.Retention.ExpiredAt), ContentReleased: r.Retention.ArtifactsReleased}
	}
	return out
}

func (s *Server) listRuns(c *call) (int, any, error) {
	q, err := s.query(c, "runs", "scope_id", "work_id", "state", "executor_binding", "executor", "work_kind", "outcome")
	if err != nil {
		return 0, nil, err
	}
	var items []keyed
	err = s.read(c.ctx, func(tx storage.Tx) error {
		runs, e := runtime.RunsInTx(tx, domain.ID(q.filters["work_id"]))
		if e != nil {
			return e
		}
		for _, r := range runs {
			outcome := ""
			if r.Execution.Outcome != nil {
				outcome = string(r.Execution.Outcome.Kind)
			}
			if !match(q, "scope_id", string(r.ScopeID)) || !match(q, "state", string(r.Execution.State)) || !match(q, "executor_binding", r.Execution.ExecutorBinding) || !match(q, "executor", r.Executor) || !match(q, "work_kind", r.WorkKind) || !match(q, "outcome", outcome) || !match(q, "work_id", string(r.Execution.WorkID)) {
				continue
			}
			items = append(items, keyed{createdKey(r.Execution.CreatedAt, r.Execution.ID), toRun(r)})
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return 200, s.page("runs", q, s.nowBound(), items), nil
}

func (s *Server) getRun(c *call) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	var r runtime.Run
	if err := s.read(c.ctx, func(tx storage.Tx) error { var e error; r, e = runtime.RunInTx(tx, domain.ID(c.params[0])); return e }); err != nil {
		return 0, nil, err
	}
	if etag(c, r.Execution.Revision) {
		return 0, nil, nil
	}
	return 200, toRun(r), nil
}

// ---- doctor ----

type wireCheck struct {
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Code   string   `json:"code"`
	Detail string   `json:"detail,omitempty"`
	Items  []string `json:"items,omitempty"`
}

func checkRank(status string) int {
	switch status {
	case runtime.CheckFail:
		return 3
	case runtime.CheckWarn:
		return 2
	}
	return 0
}

// doctor runs generic runtime checks, configuration and binding checks, and
// extension-contributed checks (exporters, host watches, connection reach).
// check_secrets=true explicitly resolves scoped references, reporting only
// names and stable codes.
func (s *Server) doctor(c *call) (int, any, error) {
	values := c.r.URL.Query()
	checkSecrets := false
	for k, v := range values {
		if k != "check_secrets" || len(v) != 1 || v[0] != "true" && v[0] != "false" {
			return 0, nil, fail(400, "invalid_filter", "doctor accepts only check_secrets=true|false")
		}
		checkSecrets = v[0] == "true"
	}
	checks := runtime.Diagnose(c.ctx, s.o.Store, s.o.Now(), runtime.DoctorOptions{Ingress: s.o.Ingress})
	checks = append(checks, s.configChecks(c.ctx, checkSecrets)...)
	if s.o.Scheduler != nil {
		checks = append(checks, s.o.Scheduler.PinnedExecutorCheck(c.ctx))
	}
	for _, check := range s.o.Checks {
		if check != nil {
			checks = append(checks, check(c.ctx))
		}
	}
	if checkSecrets {
		for _, check := range s.o.DeepChecks {
			if check != nil {
				checks = append(checks, check(c.ctx))
			}
		}
	}
	state, out := runtime.CheckOK, []wireCheck{}
	for _, ch := range checks {
		if checkRank(ch.Status) > checkRank(state) {
			state = ch.Status
		}
		out = append(out, wireCheck{Name: ch.Name, Status: ch.Status, Code: ch.Code, Detail: ch.Detail, Items: ch.Items})
	}
	return 200, map[string]any{"state": state, "checks": out}, nil
}

func (s *Server) configChecks(ctx context.Context, checkSecrets bool) []runtime.Check {
	snapshot, generation := s.o.Config.Current()
	var checks []runtime.Check
	cfg := runtime.Check{Name: "config", Status: runtime.CheckOK, Code: "current", Detail: "generation " + strconv.FormatUint(generation, 10) + " is active and matches the config file"}
	switch s.o.Config.DiskStatus() {
	case "pending_edit":
		cfg.Status, cfg.Code, cfg.Detail = runtime.CheckWarn, "pending_edit", "the config file differs from active generation "+strconv.FormatUint(generation, 10)+"; validate and apply it"
	case "invalid":
		cfg.Status, cfg.Code, cfg.Detail = runtime.CheckWarn, "disk_invalid", "the config file is invalid; generation "+strconv.FormatUint(generation, 10)+" keeps running"
	case "unreadable":
		cfg.Status, cfg.Code, cfg.Detail = runtime.CheckWarn, "disk_unreadable", "the config file cannot be read safely; generation "+strconv.FormatUint(generation, 10)+" keeps running"
	}
	checks = append(checks, cfg)
	if snapshot == nil {
		return append(checks, runtime.Check{Name: "bindings", Status: runtime.CheckFail, Code: "no_generation", Detail: "no configuration generation is active"})
	}
	registry := s.o.Config.Registry()
	var broken []string
	bindings := 0
	for _, id := range snapshot.Order {
		scope := snapshot.Scopes[id]
		for _, b := range scope.Bindings {
			bindings++
			e := registry.Executors[b.Executor]
			if e == nil {
				broken = append(broken, fmt.Sprintf("%s/%s: executor %s not registered", id, b.Name, b.Executor))
				continue
			}
			if err := extension.ValidateBinding(domain.Binding{Name: b.Name, Executor: b.Executor, Requires: b.Requires}, e); err != nil {
				broken = append(broken, fmt.Sprintf("%s/%s: capabilities not satisfied", id, b.Name))
			}
		}
	}
	if len(broken) > 0 {
		sort.Strings(broken)
		checks = append(checks, runtime.Check{Name: "bindings", Status: runtime.CheckFail, Code: "binding_unavailable", Detail: fmt.Sprintf("%d binding(s) cannot run", len(broken)), Items: broken})
	} else {
		checks = append(checks, runtime.Check{Name: "bindings", Status: runtime.CheckOK, Code: "registered", Detail: fmt.Sprintf("%d binding(s) resolve to registered executors with their required capabilities", bindings)})
	}
	conn := runtime.Check{Name: "connections", Status: runtime.CheckOK, Code: "declared", Detail: fmt.Sprintf("%d connection(s) declared; reachability checks belong to their extensions", len(snapshot.Connections))}
	var unregistered []string
	for _, cn := range snapshot.Connections {
		if _, ok := registry.Connections[cn.Type]; !ok && registry.Connections != nil {
			unregistered = append(unregistered, cn.ID+": type "+cn.Type+" not registered")
		}
	}
	if len(unregistered) > 0 {
		conn.Status, conn.Code, conn.Detail, conn.Items = runtime.CheckFail, "type_unregistered", fmt.Sprintf("%d connection(s) have no registered type", len(unregistered)), unregistered
	}
	checks = append(checks, conn)
	if !checkSecrets {
		return append(checks, runtime.Check{Name: "secrets", Status: runtime.CheckInfo, Code: "not_checked", Detail: "secret references are resolved only with check_secrets=true"})
	}
	if s.o.Resolver == nil {
		return append(checks, runtime.Check{Name: "secrets", Status: runtime.CheckWarn, Code: "resolver_unavailable", Detail: "no secret resolver is configured for doctor"})
	}
	var failed []string
	total := 0
	for _, cn := range snapshot.Connections {
		for _, name := range sortedKeys(cn.SecretRefs) {
			total++
			if _, err := s.o.Resolver.Resolve(ctx, cn.SecretRefs[name]); err != nil {
				failed = append(failed, fmt.Sprintf("connection %s secret %s: %s", cn.ID, name, config.SecretCode(err)))
			}
		}
	}
	for _, id := range snapshot.Order {
		scope := snapshot.Scopes[id]
		for _, name := range sortedKeys(scope.SecretRefs) {
			if scope.SecretRefs[name] == "" {
				continue
			}
			total++
			if _, err := s.o.Resolver.Resolve(ctx, scope.SecretRefs[name]); err != nil {
				failed = append(failed, fmt.Sprintf("scope %s secret %s: %s", id, name, config.SecretCode(err)))
			}
		}
	}
	if len(failed) > 0 {
		return append(checks, runtime.Check{Name: "secrets", Status: runtime.CheckFail, Code: "unresolved", Detail: fmt.Sprintf("%d of %d secret reference(s) failed to resolve", len(failed), total), Items: failed})
	}
	return append(checks, runtime.Check{Name: "secrets", Status: runtime.CheckOK, Code: "resolved", Detail: fmt.Sprintf("%d secret reference(s) resolved; values were discarded", total)})
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
