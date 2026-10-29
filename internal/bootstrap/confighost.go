package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/internal/cli"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// configHost is inproc.ConfigHost over the daemon's config service: it
// reads, validates and edits config text, and never applies it.
type configHost struct {
	configs func() *management.ConfigService
	control func() *runtime.Control
	path    string
	def     func() []byte
}

var _ inproc.ConfigHost = configHost{}

func (d *daemon) configHost() configHost {
	return configHost{configs: func() *management.ConfigService { return d.configs }, path: d.o.ConfigPath, def: DefaultConfig,
		control: func() *runtime.Control {
			if c := d.current.Load(); c != nil {
				return c.control
			}
			return nil
		}}
}

func (h configHost) Base(context.Context) (inproc.ConfigBase, error) {
	c := h.configs()
	if c == nil {
		return inproc.ConfigBase{}, errors.New("no configuration yet")
	}
	s, generation := c.Current()
	if s == nil {
		return inproc.ConfigBase{}, errors.New("no configuration yet")
	}
	text, err := config.ReadFile(h.path)
	if errors.Is(err, fs.ErrNotExist) {
		text, err = h.def(), nil
	}
	if err != nil {
		return inproc.ConfigBase{}, errors.New("the config file could not be read")
	}
	b := inproc.ConfigBase{Generation: strconv.FormatUint(generation, 10), Hash: s.Hash, Text: text}
	for id, scope := range s.Scopes {
		if scope.Source != "" {
			b.Folders = append(b.Folders, string(id))
		}
	}
	sort.Strings(b.Folders)
	return b, nil
}

func (h configHost) Validate(_ context.Context, text []byte) (string, []inproc.ConfigDiagnostic, error) {
	c := h.configs()
	if c == nil {
		return "", nil, errors.New("no configuration yet")
	}
	s, err := c.Validate(text)
	var invalid *config.ValidationError
	switch {
	case errors.As(err, &invalid):
		out := make([]inproc.ConfigDiagnostic, len(invalid.Diagnostics))
		for i, d := range invalid.Diagnostics {
			out[i] = inproc.ConfigDiagnostic{Path: d.Path, Code: d.Code, Message: d.Message}
		}
		return "", out, nil
	case err != nil:
		return "", []inproc.ConfigDiagnostic{{Path: "", Code: "invalid", Message: "the configuration is not valid JSON"}}, nil
	}
	return s.Hash, nil, nil
}

func (configHost) Edit(text []byte, op, pointer string, value json.RawMessage) ([]byte, json.RawMessage, error) {
	doc, err := cli.ParseConfigDoc(text)
	if err != nil {
		return nil, nil, errors.New("the config is not a JSON object")
	}
	var before json.RawMessage
	if !strings.HasSuffix(pointer, "/-") {
		if b, err := doc.Raw(pointer); err == nil {
			before = b
		}
	}
	switch op {
	case "add":
		err = doc.Set(pointer, string(value))
	case "replace":
		if before == nil {
			return nil, nil, errors.New("nothing is at " + pointer + " to replace")
		}
		err = doc.Set(pointer, string(value))
	case "remove":
		if before == nil {
			return nil, nil, errors.New("nothing is at " + pointer + " to remove")
		}
		err = doc.Unset(pointer)
	default:
		return nil, nil, errors.New("op is add, replace or remove")
	}
	if err != nil {
		return nil, nil, err
	}
	return doc.Bytes(), before, nil
}

// configCandidate is the configuration a Work proposes, from the in-process
// extension that owns its kind (GET /work/{id}/config-candidate).
func (d *daemon) configCandidate(ctx context.Context, w domain.Work) (extension.Candidate, error) {
	for _, x := range d.inproc {
		if c, ok := x.(extension.ConfigCandidate); ok && (ownership{x: x}).workKind(w.Kind) {
			return c.ConfigCandidate(ctx, w)
		}
	}
	return extension.Candidate{}, storage.ErrNotFound
}

func (h configHost) Trace(ctx context.Context, text []byte, scope, sample string) (inproc.CandidateTrace, error) {
	c, control := h.configs(), h.control()
	if c == nil || control == nil {
		return inproc.CandidateTrace{}, errors.New("no configuration yet")
	}
	s, err := c.Validate(text)
	if err != nil {
		return inproc.CandidateTrace{}, errors.New("the candidate doesn't validate; validate it to see why")
	}
	if _, ok := s.Scope(domain.ID(scope)); !ok {
		return inproc.CandidateTrace{}, errors.New("the candidate has no scope " + scope)
	}
	scopes := s.DomainScopes()
	samples, err := control.SamplesCandidate(ctx, domain.ID(scope), scopes)
	if err != nil {
		return inproc.CandidateTrace{}, errors.New("the scope's samples can't be listed")
	}
	var out inproc.CandidateTrace
	var chosen *runtime.TraceSample
	for i, x := range samples {
		id := x.Handler + "/" + x.ID
		out.Samples = append(out.Samples, inproc.CandidateSample{ID: id, Title: x.Title})
		if id == sample || x.ID == sample {
			chosen = &samples[i]
		}
	}
	if sample == "" {
		return out, nil
	}
	if chosen == nil {
		return out, errors.New("the scope has no sample " + sample)
	}
	now := time.Now()
	e := chosen.Event
	e.ID, e.DeliveryID, e.OccurredAt, e.ReceivedAt = "trace", "trace", now, now
	if err = runtime.CheckTraceEvent(e); err != nil {
		return out, errors.New("the sample can't be traced")
	}
	t, err := control.TraceCandidate(ctx, domain.ID(scope), e, scopes)
	if err != nil {
		return out, errors.New("the trace failed")
	}
	out.Samples, out.Sample = nil, chosen.Handler+"/"+chosen.ID
	for _, r := range t.Routes {
		out.Routes = append(out.Routes, inproc.CandidateRoute{Route: r.Route, Handler: r.Handler, Binding: r.Binding, Verdict: r.Verdict, Reason: r.Reason})
	}
	out.Effect = inproc.CandidateEffect{Kind: t.Effect.Kind, WorkID: string(t.Effect.WorkID), Binding: t.Effect.Binding, Detail: t.Effect.Detail}
	return out, nil
}
