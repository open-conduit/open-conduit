package management

import (
	"context"
	"errors"
	"sort"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/internal/runtime"
	"github.com/alsiola/open-conduit/storage"
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
// to a Work's executor-private session. It carries no environment: the
// operator's own shell supplies it.
type AttachSpec struct {
	Argv []string `json:"argv"`
	Dir  string   `json:"dir,omitempty"`
}

var (
	// ErrAttachUnsupported means the Work's executor offers no attach.
	ErrAttachUnsupported = errors.New("executor does not support attach")
	// ErrNoSession means the executor has no session to attach to yet.
	ErrNoSession = errors.New("no session to attach to")
)

type wireHold struct {
	EventID string `json:"event_id"`
	Reason  string `json:"reason,omitempty"`
	At      string `json:"at"`
}

// workDetail is GET /work/{id}: the primitive plus its operator hold and
// extension annotations.
type workDetail struct {
	wireWork
	Hold        *wireHold    `json:"hold,omitempty"`
	Annotations []Annotation `json:"annotations,omitempty"`
}

func (s *Server) detail(ctx context.Context, w domain.Work) (workDetail, error) {
	out := workDetail{wireWork: toWork(w)}
	err := s.read(ctx, func(tx storage.Tx) error {
		h, held, e := runtime.HoldInTx(tx, w.ID)
		if held {
			out.Hold = &wireHold{EventID: string(h.EventID), Reason: h.Reason, At: ts(h.At)}
		}
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
// releasing it lets the next attempt continue that session.
func (s *Server) attachWork(c *call) (int, any, error) {
	var in struct{}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	if s.o.Attach == nil {
		return 0, nil, ErrAttachUnsupported
	}
	var w domain.Work
	var executor string
	err := s.read(c.ctx, func(tx storage.Tx) error {
		var e error
		if w, e = tx.Work(domain.ID(c.params[0])); e != nil {
			return e
		}
		if _, held, e := runtime.HoldInTx(tx, w.ID); e != nil {
			return e
		} else if !held {
			return fail(409, "not_held", "hold the Work before attaching (open-conduit work attach does this)")
		}
		xs, e := tx.ListExecutions(w.ID)
		if e != nil {
			return e
		}
		var latest uint32
		for _, x := range xs {
			if x.State == domain.ExecutionPending || x.State == domain.ExecutionRunning {
				return fail(409, "attempt_running", "an attempt is running; wait for it or cancel it first")
			}
			if x.Attempt >= latest {
				latest = x.Attempt
				if st, er := runtime.AttemptStateInTx(tx, x.ID); er == nil {
					executor = st.Executor
				} else if !errors.Is(er, storage.ErrNotFound) {
					return er
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	if executor == "" {
		return 0, nil, ErrNoSession
	}
	spec, err := s.o.Attach(c.ctx, executor, w)
	if err != nil {
		return 0, nil, err
	}
	if len(spec.Argv) == 0 {
		return 0, nil, ErrNoSession
	}
	return 200, map[string]any{"work_id": string(w.ID), "executor": executor, "argv": spec.Argv, "dir": spec.Dir}, nil
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
