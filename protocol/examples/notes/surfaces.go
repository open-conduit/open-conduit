package notes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/storage"
)

// The surfaces 07-03 adapted, so the example covers every host-to-extension
// method a package can serve: a webhook receiver, a resource provider, a
// telemetry exporter, Work annotations and a setup flow.

const (
	// Scratch is the example resource provider: a scratch directory per
	// Work (mode work, released once the Work is done) or per execution
	// (mode fresh, released once the execution is done). Its config may
	// name the default mode ({"mode": "fresh"}) and link the scope's shared
	// cache directory into each scratch directory ({"cache": true}), which a
	// sandboxed executor could only use by writing outside its sandbox.
	Scratch   = "notes-scratch"
	ScratchNS = "notes.scratch"
	// WebhookToken is the toy shared secret a webhook request carries in
	// X-Notes-Token (a real extension verifies a signature).
	WebhookToken = "notes-webhook"
)

// webhook accepts a note push over HTTP for a source: the body is a push
// delivery body and X-Notes-Delivery its delivery ID. It answers 202 only
// after the host's durable acceptance.
func webhook(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("X-Notes-Token") != WebhookToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || r.Header.Get("X-Notes-Delivery") == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		h := sdk.HostFrom(r.Context())
		accepted, err := h.Deliver(r.Context(), name, protocol.Delivery{ID: r.Header.Get("X-Notes-Delivery"), ReceivedAt: time.Now().UTC(), Body: body})
		if err != nil {
			http.Error(w, "not accepted", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "%d accepted\n", len(accepted))
	})
}

// scratch is the example resource provider.
type scratch struct{ stateDir func() string }

var (
	_ extension.ResourceModeDefaulter  = scratch{}
	_ extension.ResourceSandboxChecker = scratch{}
)

type lease struct {
	Owner protocol.ResourceOwner `json:"owner"`
	Dir   string                 `json:"dir"`
	Mode  string                 `json:"mode,omitempty"`
}

// scratchConfig is a binding's scratch resource config.
type scratchConfig struct {
	Mode  string `json:"mode,omitempty"`
	Cache bool   `json:"cache,omitempty"`
}

// parseScratch reads a scratch config strictly: only the modes it supports.
func parseScratch(raw json.RawMessage) (scratchConfig, error) {
	var c scratchConfig
	if len(raw) > 0 {
		d := json.NewDecoder(strings.NewReader(string(raw)))
		d.DisallowUnknownFields()
		if err := d.Decode(&c); err != nil {
			return c, err
		}
	}
	if c.Mode != "" && c.Mode != extension.ResourceWork && c.Mode != extension.ResourceFresh {
		return c, errors.New("scratch mode is work or fresh")
	}
	return c, nil
}

// ModeDefault is the mode the config itself names, "" for none.
func (scratch) ModeDefault(raw json.RawMessage) string {
	c, _ := parseScratch(raw)
	return c.Mode
}

// SandboxWorkspace refuses the shared cache: every Work's process writes it
// through the link, outside its own sandbox.
func (scratch) SandboxWorkspace(raw json.RawMessage, _ string) error {
	if c, _ := parseScratch(raw); c.Cache {
		return errors.New("a scratch cache is shared by every Work outside the sandbox; remove cache")
	}
	return nil
}

func scratchRef(o extension.ResourceOwner, mode string) string {
	ref := string(o.WorkID)
	if mode == extension.ResourceFresh {
		ref += "." + string(o.ExecutionID)
	}
	return strings.NewReplacer("/", "_", "..", "_").Replace(ref)
}

func (s scratch) Acquire(ctx context.Context, a extension.ResourceAcquisition) (extension.Resource, error) {
	h := sdk.HostFrom(ctx)
	if h == nil || s.stateDir == nil || s.stateDir() == "" {
		return extension.Resource{}, extension.ErrUnsupported
	}
	cfg, err := parseScratch(a.Config.Data)
	if err != nil {
		return extension.Resource{}, extension.ErrUnsupported
	}
	mode := a.Request.Mode
	if mode == "" {
		mode = cfg.Mode
	}
	switch {
	case mode == "":
		mode = extension.ResourceWork
	case mode != extension.ResourceWork && mode != extension.ResourceFresh:
		return extension.Resource{}, extension.ErrUnsupported
	}
	if cfg.Cache && a.Request.Sandboxed {
		return extension.Resource{}, extension.ErrUnsupported
	}
	ref := scratchRef(a.Owner, mode)
	dir := filepath.Join(s.stateDir(), "scratch", ref)
	// The lease is recorded before anything is created, so reconcile finds
	// it after a crash at any point.
	raw, _ := json.Marshal(lease{Owner: protocol.FromOwner(a.Owner), Dir: dir, Mode: mode})
	if err := h.Update(ctx, ScratchNS, ref, func(old []byte, found bool) ([]byte, error) {
		if found {
			return old, nil
		}
		return raw, nil
	}); err != nil {
		return extension.Resource{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return extension.Resource{Kind: Scratch, Ref: ref, Dir: dir}, extension.ErrResourceSetup
	}
	if cfg.Cache {
		cache := filepath.Join(s.stateDir(), "cache", strings.NewReplacer("/", "_", "..", "_").Replace(string(a.Owner.ScopeID)))
		if err := os.MkdirAll(cache, 0o700); err != nil {
			return extension.Resource{Kind: Scratch, Ref: ref, Dir: dir}, extension.ErrResourceSetup
		}
		if err := os.Symlink(cache, filepath.Join(dir, "cache")); err != nil && !os.IsExist(err) {
			return extension.Resource{Kind: Scratch, Ref: ref, Dir: dir}, extension.ErrResourceSetup
		}
	}
	if a.Request.Log != nil {
		fmt.Fprintf(a.Request.Log, "scratch directory %s\n", ref)
	}
	return extension.Resource{Kind: Scratch, Ref: ref, Dir: dir, Summary: "scratch " + ref}, nil
}

func (s scratch) Lookup(ctx context.Context, o extension.ResourceOwner, ref string) (extension.Resource, error) {
	h := sdk.HostFrom(ctx)
	raw, _, err := h.Get(ctx, ScratchNS, ref)
	if errors.Is(err, storage.ErrNotFound) {
		return extension.Resource{}, extension.ErrResourceNotFound
	}
	if err != nil {
		return extension.Resource{}, err
	}
	var l lease
	if json.Unmarshal(raw, &l) != nil || l.Owner.WorkID != string(o.WorkID) {
		return extension.Resource{}, extension.ErrResourceNotFound
	}
	return extension.Resource{Kind: Scratch, Ref: ref, Dir: l.Dir}, nil
}

func (s scratch) Reconcile(ctx context.Context, status extension.ResourceStatusFunc) (extension.ResourceReport, error) {
	h := sdk.HostFrom(ctx)
	entries, err := h.List(ctx, ScratchNS, "")
	if err != nil {
		return extension.ResourceReport{}, err
	}
	var rep extension.ResourceReport
	for _, e := range entries {
		var l lease
		if json.Unmarshal(e.Value, &l) != nil {
			continue
		}
		st, err := status(ctx, l.Owner.Extension())
		if err != nil {
			return rep, err
		}
		done := st.WorkDone
		if l.Mode == extension.ResourceFresh {
			done = st.ExecutionDone
		}
		if st.Known && !done {
			rep.Active++
			continue
		}
		mode := l.Mode
		if mode == "" {
			mode = extension.ResourceWork
		}
		if err := os.RemoveAll(l.Dir); err != nil {
			rep.Attention = append(rep.Attention, extension.ResourceLeaseReport{Ref: e.Key, Mode: mode, State: "releasing", Code: "cleanup_failed"})
			continue
		}
		if err := h.Commit(ctx, protocol.KVCommitParams{Checks: []protocol.KVCheck{{Namespace: ScratchNS, Key: e.Key, Version: e.Version}}, Deletes: []protocol.KVDelete{{Namespace: ScratchNS, Key: e.Key}}}); err != nil {
			return rep, err
		}
		rep.Released++
	}
	return rep, nil
}

// exporter appends each telemetry record's name to telemetry.log in the
// state directory.
type exporter struct{ stateDir func() string }

func (x exporter) Export(_ context.Context, records []observability.Record) error {
	if x.stateDir == nil || x.stateDir() == "" {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(x.stateDir(), "telemetry.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, r := range records {
		fmt.Fprintf(f, "%s %s\n", r.Name, r.Correlation.WorkID)
	}
	return nil
}
func (exporter) Shutdown(context.Context) error { return nil }

// annotate shows a note's indexed text on its Work.
func annotate(ctx context.Context, h *sdk.Host, w domain.Work) ([]protocol.Annotation, error) {
	out, err := annotateNote(ctx, h, w)
	if err == nil && w.Display != nil {
		// What the Work's source said it is, which the host sends only
		// for Work in this extension's scopes.
		out = append(out, protocol.Annotation{Name: "shown", Value: w.Display.Title})
	}
	return out, err
}

func annotateNote(ctx context.Context, h *sdk.Host, w domain.Work) ([]protocol.Annotation, error) {
	for _, k := range w.CorrelationKeys {
		if k.Source != Name {
			continue
		}
		v, _, err := h.Get(ctx, IndexNS, k.ExternalID)
		if errors.Is(err, storage.ErrNotFound) {
			return []protocol.Annotation{{Name: "note", Value: k.ExternalID + " (not indexed yet)"}}, nil
		}
		if err != nil {
			return nil, err
		}
		return []protocol.Annotation{{Name: "note", Value: indexText(v)}}, nil
	}
	return nil, nil
}

// Setup is the example `open-conduit init notes` flow: it asks for the
// note service file and adds a notes scope.
type Setup struct{}

func (Setup) SetupInfo() extension.SetupInfo {
	return extension.SetupInfo{Type: Name, Summary: "add a scope watching a note service file", Usage: "[--service FILE]"}
}

func (Setup) Setup(_ context.Context, host extension.SetupHost, args []string) (extension.SetupResult, error) {
	service := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--service" {
			service = args[i+1]
		}
	}
	if service == "" {
		var err error
		if service, err = host.Prompt("Note service file", extension.Prompt{Default: "notes.json"}); err != nil {
			return extension.SetupResult{}, err
		}
	}
	if !filepath.IsAbs(service) {
		service = filepath.Join(host.Cwd(), service)
	}
	host.Printf("Watching %s\n", service)
	scope, _ := json.Marshal(map[string]any{"id": "notes", "name": "Notes", "enabled": true,
		"extensions": []any{map[string]any{"name": "notes", "type": Name, "config": map[string]string{"service": service}, "secret_names": []string{}}}})
	return extension.SetupResult{Scope: scope, Summary: []string{"notes scope watching " + service}}, nil
}

// Actions is the example action provider: on Work that follows a note and
// is still open, the local operator may add a remark, which becomes a
// notes.note.annotated Event on the note. It also shows that Work a panel
// naming the note, with the remark button. The same value serves in-process
// and behind work.actions, work.act and view.render.
type Actions struct{}

var (
	_ extension.ActionProvider = Actions{}
	_ extension.PanelProvider  = Actions{}
)

func (Actions) Panels(_ context.Context, t extension.ActionTarget) ([]extension.Panel, error) {
	k, ok := noteKey(t.Work)
	if !ok {
		return nil, nil
	}
	return []extension.Panel{{ID: "note", Title: "Note " + k.ExternalID, Sections: []extension.PanelSection{
		{Fields: []extension.PanelField{{Name: "note", Value: k.ExternalID}}, Actions: []string{"remark"}},
	}}}, nil
}

// noteKey is the note a live Work follows, if any.
func noteKey(w *domain.Work) (domain.CorrelationKey, bool) {
	if w == nil || w.State.Terminal() {
		return domain.CorrelationKey{}, false
	}
	for _, k := range w.CorrelationKeys {
		if k.Source == Name {
			return k, true
		}
	}
	return domain.CorrelationKey{}, false
}

func (Actions) Actions(_ context.Context, t extension.ActionTarget) ([]extension.Action, error) {
	if _, ok := noteKey(t.Work); !ok {
		return nil, nil
	}
	return []extension.Action{{ID: "remark", Label: "Add a remark to the note", Prompt: "remark"}}, nil
}

func (Actions) Invoke(_ context.Context, c extension.ActionCall) (extension.ActionPlan, error) {
	if c.Action != "remark" {
		return extension.ActionPlan{}, &extension.OperationError{Code: "unknown_action", Message: "notes offers no action " + c.Action}
	}
	k, ok := noteKey(c.Target.Work)
	if !ok {
		return extension.ActionPlan{}, &extension.OperationError{Code: "not_outstanding", Message: "this Work no longer follows an open note"}
	}
	if c.Caller.Kind != extension.CallerOperator {
		return extension.ActionPlan{}, &extension.OperationError{Code: "not_allowed", Message: "only the operator may remark on a note"}
	}
	if strings.TrimSpace(c.Input) == "" {
		return extension.ActionPlan{}, &extension.OperationError{Code: "invalid_input", Message: "a remark needs some text"}
	}
	return extension.ActionPlan{Event: domain.Event{Type: Annotated, CorrelationKey: &k}, Message: "remark recorded on note " + k.ExternalID}, nil
}

// telemetry records a pass event for each configured scope and the machine
// (telemetry.capture), then lists the host's probes and runs ProbeName
// (probe.list, probe.run), writing both to probes.json.
func telemetry(ctx context.Context, h *sdk.Host, o Options, scopes []string) error {
	sort.Strings(scopes)
	for _, scope := range append(scopes, "") {
		if err := h.Capture(ctx, domain.ID(scope), PassEvent, time.Time{}, map[string]any{"release": o.Release}); err != nil {
			return err
		}
	}
	probes, err := h.Probes(ctx)
	if err != nil {
		return err
	}
	res, err := h.Probe(ctx, ProbeName, map[string]string{"say": "hello"})
	if err != nil {
		return err
	}
	if o.StateDir == nil || o.StateDir() == "" {
		return nil
	}
	out, err := json.Marshal(map[string]any{"probes": probes, "result": res})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(o.StateDir(), "probes.json"), out, 0o600)
}

// ConfigRun is what ConfigTask writes to config.json: every answer the
// host gave it.
type ConfigRun struct {
	Base     protocol.ConfigBaseResult     `json:"base"`
	Edits    []protocol.ConfigEditResult   `json:"edits"`
	Validate protocol.ConfigValidateResult `json:"validate"`
	Samples  protocol.ConfigTraceResult    `json:"samples"`
	Trace    *protocol.ConfigTraceResult   `json:"trace,omitempty"`
}

// proposeConfig builds a candidate from the configuration now: the first
// scope renamed, with a new reference. The host holds the text; the
// package sees it only redacted, and nothing it does applies it.
func proposeConfig(ctx context.Context, h *sdk.Host, o Options, scopes []string) error {
	if len(scopes) == 0 {
		return nil
	}
	sort.Strings(scopes)
	scope := scopes[0]
	var run ConfigRun
	var err error
	if run.Base, err = h.ConfigBase(ctx); err != nil {
		return err
	}
	handle := run.Base.Handle
	for _, e := range []struct{ pointer, value string }{
		{"/scopes/" + scope + "/name", `"Notes, proposed"`},
		{"/scopes/" + scope + "/secret_refs/notes_token", `"` + ConfigRef + `"`},
	} {
		res, err := h.ConfigEdit(ctx, handle, "add", e.pointer, json.RawMessage(e.value))
		if err != nil {
			return err
		}
		run.Edits, handle = append(run.Edits, res), res.Handle
	}
	if run.Validate, err = h.ConfigValidate(ctx, handle); err != nil {
		return err
	}
	if run.Samples, err = h.ConfigTrace(ctx, handle, scope, ""); err != nil {
		return err
	}
	if len(run.Samples.Samples) > 0 {
		t, err := h.ConfigTrace(ctx, handle, scope, run.Samples.Samples[0].ID)
		if err != nil {
			return err
		}
		run.Trace = &t
	}
	cand := extension.Candidate{Handle: handle, BaseGeneration: run.Base.Generation, Hash: run.Validate.Hash, Changes: []extension.CandidateChange{
		{ID: "n1", Op: "add", Pointer: "/scopes/" + scope + "/name", State: extension.ChangeApproved, Why: "a name for what it holds", Before: run.Edits[0].Before, After: json.RawMessage(`"Notes, proposed"`)},
		{ID: "n2", Op: "add", Pointer: "/scopes/" + scope + "/secret_refs/notes_token", State: extension.ChangeApproved, Why: "the token, by reference", After: json.RawMessage(`"[redacted]"`)},
	}}
	if len(run.Validate.Diagnostics) > 0 {
		cand.Handle, cand.Hash = "", ""
	}
	b, err := json.Marshal(cand)
	if err != nil {
		return err
	}
	if err = h.Update(ctx, CandidateNS, "latest", func([]byte, bool) ([]byte, error) { return b, nil }); err != nil {
		return err
	}
	if o.StateDir == nil || o.StateDir() == "" {
		return nil
	}
	out, err := json.Marshal(run)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(o.StateDir(), "config.json"), out, 0o600)
}

// candidateFor is what a note's Work proposes: the candidate ConfigTask
// built last, approved as it stands.
func candidateFor(ctx context.Context, h *sdk.Host, w domain.Work) (extension.Candidate, error) {
	var c extension.Candidate
	if w.Kind != Kind {
		return c, storage.ErrNotFound
	}
	b, _, err := h.Get(ctx, CandidateNS, "latest")
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// appendLine appends one line to a file in the state directory.
func appendLine(stateDir func() string, file, line string) error {
	if stateDir == nil || stateDir() == "" {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(stateDir(), file), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, line)
	return err
}
