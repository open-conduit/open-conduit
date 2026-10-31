package management

import (
	"context"
	"errors"
	"sort"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// Operator controls (03-05): rebind, hold/release, attach and scope
// reconcile. Like every other control they authenticate as the operator,
// require an Idempotency-Key and commit an accepted core.manual.* Event.

// Annotation is one line of extension-contributed context for Work (for
// example the tracker item it owns or why it is held). Values are safe,
// display-only strings; core never interprets them.
type Annotation struct {
	Source string `json:"source"`
	Name   string `json:"name"`
	Value  string `json:"value"`
}

// AttachSpec is an interactive command an operator runs locally to attach
// to a Work's executor-private continuity. It carries no environment: the
// operator's own shell supplies it.
type AttachSpec struct {
	Argv []string `json:"argv"`
	Dir  string   `json:"dir,omitempty"`
}

var (
	// ErrAttachUnsupported means the Work's executor offers no attach.
	ErrAttachUnsupported = errors.New("executor does not support attach")
	// ErrNothingToAttach means the executor has nothing to attach to yet
	// (extension.ErrNothingToAttach).
	ErrNothingToAttach = extension.ErrNothingToAttach
	// ErrAttachSandboxed means the Work ran in a sandbox (B09). An attach
	// runs on the host, outside it, so it is refused.
	ErrAttachSandboxed = errors.New("attach refused: the work ran in a sandbox")
	// ErrLiveUnsupported means the run's executor offers no live output.
	ErrLiveUnsupported = errors.New("executor does not support live output")
	// ErrNotLive means the run is not running (extension.ErrNotLive).
	ErrNotLive = extension.ErrNotLive
)

type wireHold struct {
	EventID string `json:"event_id"`
	Reason  string `json:"reason,omitempty"`
	At      string `json:"at"`
}

// workDetail is GET /work/{id}: the primitive plus its operator hold, the
// count of routed input queued for its next turn and extension annotations.
// Ready Work with no queued input is idle: it waits for its next event, not
// for a slot.
type workDetail struct {
	wireWork
	Hold        *wireHold    `json:"hold,omitempty"`
	Inputs      int          `json:"inputs"`
	Annotations []Annotation `json:"annotations,omitempty"`
}

func (s *Server) detail(ctx context.Context, w domain.Work) (workDetail, error) {
	out := workDetail{wireWork: toWork(w)}
	err := s.read(ctx, func(tx storage.Tx) error {
		h, held, e := runtime.HoldInTx(tx, w.ID)
		if held {
			out.Hold = &wireHold{EventID: string(h.EventID), Reason: h.Reason, At: ts(h.At)}
		}
		if e != nil {
			return e
		}
		inputs, e := runtime.PendingInputsInTx(tx, w.ID)
		out.Inputs = len(inputs)
		return e
	})
	if err != nil {
		return out, err
	}
	out.Annotations = s.annotate(ctx, w)
	return out, nil
}

func (s *Server) annotate(ctx context.Context, w domain.Work) []Annotation {
	if s.o.Annotate == nil {
		return nil
	}
	var out []Annotation
	for _, a := range s.o.Annotate(ctx, w) {
		if a.Name == "" || len(a.Value) > 2048 {
			continue
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

func (s *Server) rebindWork(c *call) (int, any, error) {
	var in struct {
		ExpectedRevision string `json:"expected_revision"`
		Binding          string `json:"binding"`
		Reason           string `json:"reason"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	rev, err := parseRevision(in.ExpectedRevision)
	if err != nil {
		return 0, nil, err
	}
	if len(in.Reason) > 1024 || in.Binding == "" || len(in.Binding) > 63 {
		return 0, nil, fail(400, "malformed", "binding is required and reason must be short")
	}
	event, err := s.o.Control.Rebind(c.ctx, domain.ID(c.params[0]), rev, in.Binding, in.Reason, c.key)
	if err != nil {
		return 0, nil, err
	}
	return 202, map[string]any{"event_id": string(event), "work_id": c.params[0], "binding": in.Binding}, nil
}

func (s *Server) holdWork(c *call) (int, any, error) {
	rev, reason, err := s.control(c)
	if err != nil {
		return 0, nil, err
	}
	event, err := s.o.Control.HoldWork(c.ctx, domain.ID(c.params[0]), rev, reason, c.key)
	if err != nil {
		return 0, nil, err
	}
	return 202, map[string]any{"event_id": string(event), "work_id": c.params[0]}, nil
}

func (s *Server) releaseWork(c *call) (int, any, error) {
	rev, reason, err := s.control(c)
	if err != nil {
		return 0, nil, err
	}
	event, err := s.o.Control.ReleaseWork(c.ctx, domain.ID(c.params[0]), rev, reason, c.key)
	if err != nil {
		return 0, nil, err
	}
	return 202, map[string]any{"event_id": string(event), "work_id": c.params[0]}, nil
}

// attachWork returns the attach command for held Work with no live attempt.
// The hold keeps incoming input queued while the operator is in the session;
// releasing it lets the next attempt continue that session. Work that ran in
// a process sandbox is refused; Work that ran in a container attaches in one
// (`open-conduit container attach`), which is the only command its
// executor may return.
func (s *Server) attachWork(c *call) (int, any, error) {
	var in struct{}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	if s.o.Attach == nil {
		return 0, nil, ErrAttachUnsupported
	}
	t, err := s.attachTarget(c)
	if err != nil {
		return 0, nil, err
	}
	if t.sandboxed {
		return 0, nil, ErrAttachSandboxed
	}
	spec, err := s.o.Attach(c.ctx, t.executor, t.work)
	if err != nil {
		return 0, nil, err
	}
	if len(spec.Argv) == 0 {
		return 0, nil, ErrNothingToAttach
	}
	if t.contained && !ContainerAttachCommand(spec.Argv) {
		return 0, nil, ErrAttachSandboxed
	}
	return 200, map[string]any{"work_id": string(t.work.ID), "executor": t.executor, "argv": spec.Argv, "dir": spec.Dir}, nil
}

// ContainerAttachCommand reports whether argv is `<open-conduit> container
// attach <work>`, the attach of Work that ran in a container.
func ContainerAttachCommand(argv []string) bool {
	return len(argv) == 4 && argv[1] == "container" && argv[2] == "attach"
}

// attachTarget is held Work with no live attempt, its latest attempt's
// execution and executor, and whether any segment ran in a process sandbox
// or a container.
type attachTarget struct {
	work                 domain.Work
	executor             string
	execution            domain.ID
	sandboxed, contained bool
}

func (s *Server) attachTarget(c *call) (attachTarget, error) {
	var t attachTarget
	err := s.read(c.ctx, func(tx storage.Tx) error {
		var e error
		if t.work, e = tx.Work(domain.ID(c.params[0])); e != nil {
			return e
		}
		if _, held, e := runtime.HoldInTx(tx, t.work.ID); e != nil {
			return e
		} else if !held {
			return fail(409, "not_held", "hold the Work before attaching (open-conduit work attach does this)")
		}
		xs, e := tx.ListExecutions(t.work.ID)
		if e != nil {
			return e
		}
		var latest uint32
		for _, x := range xs {
			if x.State == domain.ExecutionPending || x.State == domain.ExecutionRunning {
				return fail(409, "attempt_running", "an attempt is running; wait for it or cancel it first")
			}
			st, er := runtime.AttemptStateInTx(tx, x.ID)
			if er != nil && !errors.Is(er, storage.ErrNotFound) {
				return er
			}
			sandboxed, contained := ranConfined(st)
			t.sandboxed, t.contained = t.sandboxed || sandboxed, t.contained || contained
			if x.Attempt >= latest {
				latest = x.Attempt
				t.executor, t.execution = st.Executor, x.ID
			}
		}
		return nil
	})
	if err == nil && t.executor == "" {
		err = ErrNothingToAttach
	}
	return t, err
}

// ranConfined reports whether any segment of the attempt was launched in a
// process sandbox (B09), and whether any was in a container. Its agent could
// then write the workspace's project config (claude-code's
// .claude/settings.json hooks, .mcp.json servers), which an attach on the
// host would load outside the boundary.
func ranConfined(st runtime.AttemptState) (sandboxed, contained bool) {
	for _, g := range st.Segments {
		sandboxed = sandboxed || g.Sandbox != nil
		contained = contained || g.Container != nil
	}
	return sandboxed, contained
}

// ContainerAttachment is an attach container made for an operator: its
// reference, and the argv and environment that start it attached on their
// terminal.
type ContainerAttachment struct {
	Ref  string   `json:"ref"`
	Argv []string `json:"argv"`
	Env  []string `json:"env,omitempty"`
}

// containerAttach makes the attach container for held Work with no live
// attempt whose latest session ran in a container (what `open-conduit
// container attach` runs).
func (s *Server) containerAttach(c *call) (int, any, error) {
	var in struct{}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	if s.o.ContainerAttach == nil {
		return 0, nil, ErrAttachUnsupported
	}
	t, err := s.attachTarget(c)
	if err != nil {
		return 0, nil, err
	}
	if t.sandboxed {
		return 0, nil, ErrAttachSandboxed
	}
	if !t.contained {
		return 0, nil, fail(409, "not_contained", "the Work did not run in a container; use open-conduit work attach")
	}
	a, err := s.o.ContainerAttach(c.ctx, t.executor, t.work, t.execution)
	if errors.Is(err, extension.ErrSandboxUnavailable) || errors.Is(err, extension.ErrSandboxDenied) || errors.Is(err, extension.ErrContainerRunning) {
		// What refused the container (a path, the daemon) is the operator's
		// to fix; it names no secret.
		return 0, nil, fail(422, "container_unavailable", err.Error())
	} else if err != nil {
		return 0, nil, err
	}
	return 200, a, nil
}

// containerDetach removes an attach container of the Work, with what was
// made for it, once the operator's session ends.
func (s *Server) containerDetach(c *call) (int, any, error) {
	var in struct {
		Ref string `json:"ref"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	if s.o.ContainerDetach == nil {
		return 0, nil, ErrAttachUnsupported
	}
	if in.Ref == "" {
		return 0, nil, fail(400, "invalid_request", "ref is required")
	}
	var w domain.Work
	if err := s.read(c.ctx, func(tx storage.Tx) error {
		var e error
		w, e = tx.Work(domain.ID(c.params[0]))
		return e
	}); err != nil {
		return 0, nil, err
	}
	if err := s.o.ContainerDetach(c.ctx, w, in.Ref); err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"work_id": string(w.ID), "ref": in.Ref, "removed": true}, nil
}

func (s *Server) reconcileScope(c *call) (int, any, error) {
	var in struct {
		ExpectedRevision string `json:"expected_revision"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	rev, err := parseRevision(in.ExpectedRevision)
	if err != nil {
		return 0, nil, err
	}
	event, sources, err := s.o.Control.RequestReconcile(c.ctx, domain.ID(c.params[0]), rev, c.key)
	if err != nil {
		return 0, nil, err
	}
	if sources == nil {
		sources = []string{}
	}
	if s.o.Nudge != nil {
		s.o.Nudge()
	}
	return 202, map[string]any{"event_id": string(event), "scope_id": c.params[0], "sources": sources}, nil
}
