package bootstrap

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
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
// reads, validates and edits config text it keeps behind handles, hands
// extensions only redacted copies, and never applies it.
type configHost struct {
	configs func() *management.ConfigService
	control func() *runtime.Control
	path    string
	def     func() []byte
	handles *configHandles
}

var _ inproc.ConfigHost = configHost{}

func (d *daemon) configHost() configHost {
	return configHost{configs: func() *management.ConfigService { return d.configs }, path: d.o.ConfigPath, def: func() []byte { return d.defaultDoc }, handles: &d.configHandles,
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
	b := inproc.ConfigBase{Generation: strconv.FormatUint(generation, 10), Hash: s.Hash, Handle: h.handles.put(text), Text: redactText(text)}
	for id, scope := range s.Scopes {
		if scope.Source != "" {
			b.Folders = append(b.Folders, string(id))
		}
	}
	sort.Strings(b.Folders)
	return b, nil
}

func (h configHost) Validate(_ context.Context, handle string) (string, []inproc.ConfigDiagnostic, error) {
	text, ok := h.handles.get(handle)
	if !ok {
		return "", nil, inproc.ErrConfigHandle
	}
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

func (h configHost) Edit(_ context.Context, handle, op, pointer string, value json.RawMessage) (inproc.ConfigEdited, error) {
	text, ok := h.handles.get(handle)
	if !ok {
		return inproc.ConfigEdited{}, inproc.ErrConfigHandle
	}
	out, before, err := edit(text, op, pointer, value)
	if err != nil {
		return inproc.ConfigEdited{}, err
	}
	return inproc.ConfigEdited{Handle: h.handles.put(out), Text: redactText(out), Before: redactAt(pointer, before)}, nil
}

// edit makes one edit to config text: the new text and the value that was
// at pointer.
func edit(text []byte, op, pointer string, value json.RawMessage) ([]byte, json.RawMessage, error) {
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

// configCandidate is the configuration a Work proposes, from the extension
// that owns its kind, in-process or installed (GET
// /work/{id}/config-candidate): its changes for review and, once one is
// approved, the text behind the candidate's handle, for the operator alone.
func (d *daemon) configCandidate(ctx context.Context, w domain.Work) (management.ConfigCandidate, error) {
	for _, x := range d.inproc {
		c, ok := x.(extension.ConfigCandidate)
		if !ok || !ownsKind(x, w.Kind) {
			continue
		}
		cand, err := c.ConfigCandidate(ctx, w)
		if err != nil {
			return management.ConfigCandidate{}, err
		}
		out := management.ConfigCandidate{BaseGeneration: cand.BaseGeneration, Changes: cand.Changes, ApproveAll: cand.ApproveAll, Validate: cand.Validate}
		if cand.Handle != "" {
			text, ok := d.configHandles.get(cand.Handle)
			if !ok {
				return management.ConfigCandidate{}, storage.ErrNotFound
			}
			out.Text, out.Hash = text, cand.Hash
		}
		return out, nil
	}
	return management.ConfigCandidate{}, storage.ErrNotFound
}

// ownsKind reports whether x owns the Work kind: one it declares, in
// process or in its manifest.
func ownsKind(x inproc.Extension, kind string) bool {
	if p, ok := x.(*installed); ok {
		return p.ownsKind(kind)
	}
	return ownership{x: x}.workKind(kind)
}

// pages are every extension's pages, in-process and installed, by the
// extension that declares them. A page that doesn't validate, or whose ID
// an earlier extension took, is left out.
func (d *daemon) pages() []management.Page {
	var out []management.Page
	taken := map[string]bool{}
	for _, x := range d.inproc {
		p, ok := x.(inproc.Paged)
		if !ok {
			continue
		}
		for i, pg := range p.Pages() {
			if i == extension.MaxPages || pg.Validate() != nil || taken[pg.ID] {
				continue
			}
			taken[pg.ID] = true
			out = append(out, management.Page{Extension: x.Name(), Page: pg})
		}
	}
	return out
}

func (h configHost) Trace(ctx context.Context, handle, scope, sample string) (inproc.CandidateTrace, error) {
	text, ok := h.handles.get(handle)
	if !ok {
		return inproc.CandidateTrace{}, inproc.ErrConfigHandle
	}
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

// maxConfigHandles bounds the config texts the host holds for extensions;
// the least recently used goes first.
const maxConfigHandles = 256

// configHandles holds config texts (bases and candidates) behind random
// handles, so an extension refers to text it never receives. The same text
// keeps its handle while it is held.
type configHandles struct {
	mu     sync.Mutex
	byID   map[string][]byte
	byText map[[sha256.Size]byte]string
	order  []string // least recently used first
}

func (h *configHandles) put(text []byte) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byID == nil {
		h.byID, h.byText = map[string][]byte{}, map[[sha256.Size]byte]string{}
	}
	sum := sha256.Sum256(text)
	id, ok := h.byText[sum]
	if !ok {
		var b [16]byte
		_, _ = rand.Read(b[:])
		id = "cfg_" + hex.EncodeToString(b[:])
		h.byID[id], h.byText[sum] = bytes.Clone(text), id
	}
	h.touch(id)
	for len(h.order) > maxConfigHandles {
		old := h.order[0]
		h.order = h.order[1:]
		delete(h.byText, sha256.Sum256(h.byID[old]))
		delete(h.byID, old)
	}
	return id
}

func (h *configHandles) get(id string) ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	text, ok := h.byID[id]
	if ok {
		h.touch(id)
	}
	return bytes.Clone(text), ok
}

func (h *configHandles) touch(id string) {
	if i := slices.Index(h.order, id); i >= 0 {
		h.order = slices.Delete(h.order, i, i+1)
	}
	h.order = append(h.order, id)
}

// redacted stands in for reference text.
const redacted = "[redacted]"

// redactText is config text with every secret_refs value replaced by
// "[redacted]" (false, which unsets an inherited name, is kept). Text that
// isn't JSON gives nothing.
func redactText(text []byte) []byte {
	var v any
	if json.Unmarshal(text, &v) != nil {
		return nil
	}
	out, _ := json.MarshalIndent(redactValue(v, false), "", "  ")
	return out
}

// redactAt redacts a value found at pointer: the reference itself when
// pointer is inside a secret_refs object, its values when pointer is one.
func redactAt(pointer string, value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(value, &v) != nil {
		return nil
	}
	parts := strings.Split(pointer, "/")
	switch {
	case len(parts) >= 3 && parts[len(parts)-2] == "secret_refs":
		v = redactRef(v)
	default:
		v = redactValue(v, parts[len(parts)-1] == "secret_refs")
	}
	out, _ := json.Marshal(v)
	return out
}

func redactValue(v any, refs bool) any {
	switch x := v.(type) {
	case map[string]any:
		for k, m := range x {
			if refs {
				x[k] = redactRef(m)
				continue
			}
			x[k] = redactValue(m, k == "secret_refs")
		}
	case []any:
		for i, m := range x {
			x[i] = redactValue(m, false)
		}
	}
	return v
}

func redactRef(v any) any {
	if b, ok := v.(bool); ok && !b {
		return false
	}
	return redacted
}
