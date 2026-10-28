// Package weakened holds the certification runner's negative controls
// (07-03): the example extension and executor, each with exactly one
// defect, packaged as installable extensions. Every mode must fail the
// certification criterion it names (and the runner's own tests prove it
// does), so a runner that stopped checking would be caught.
package weakened

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/examples/notes"
	"github.com/open-conduit/open-conduit/protocol/examples/runner"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// Mode is one negative control: the base example it weakens and the
// certification criterion it must fail.
type Mode struct {
	Name, Base, Fails, Defect string
}

// Modes are every negative control.
var Modes = []Mode{
	{"version-blind", notes.Name, "handshake.version_refusal", "answers initialize with version 1 whatever the host offers"},
	{"manifest-drift", notes.Name, "handshake.manifest", "announces a manifest that differs from its file"},
	{"stdout-noise", notes.Name, "handshake.manifest", "prints a diagnostic line on stdout"},
	{"no-eof-exit", notes.Name, "lifecycle.stdin_eof", "keeps running after its stdin closes"},
	{"unstable-push", notes.Name, "ingress.conformance", "stamps push observations with the time of the call"},
	{"route-drift", notes.Name, "routes.decisions", "creates Work under an ID that changes on every call"},
	{"display-link", notes.Name, "routes.decisions", "links a Work's display to a javascript: URL"},
	{"display-overlong", notes.Name, "routes.decisions", "gives a Work a display title over the length limit"},
	{"operation-drift", notes.Name, "operations.cases", "answers an unknown note with a generic refusal"},
	{"migration-once", notes.Name, "storage.kv_migration", "fails a repeated KV migration (not idempotent)"},
	{"foreign-kv", notes.Name, "scope.host_surface", "tries to write another extension's KV namespace at configure"},
	{"early-ack", runner.Name, "executor.conformance", "acknowledges a run before persisting recovery metadata"},
	{"replacement-recover", runner.Name, "executor.conformance", "launches a replacement attempt from recover"},
	{"inject-unknown", runner.Name, "executor.inject", "accepts input for executions it does not hold"},
}

// Lookup returns a mode by name.
func Lookup(name string) (Mode, bool) {
	for _, m := range Modes {
		if m.Name == name {
			return m, true
		}
	}
	return Mode{}, false
}

// Manifest is the mode's manifest: the base example's, started as
// `weakened-extension MODE`.
func Manifest(m Mode) protocol.Manifest {
	var out protocol.Manifest
	if m.Base == runner.Name {
		out = runner.Manifest()
	} else {
		out = notes.Manifest(notes.Options{Release: 2})
	}
	out.Command = []string{"weakened-extension", m.Name}
	if m.Name == "version-blind" {
		// It claims to speak a version it does not.
		out.Protocol = []int{protocol.Version}
	}
	return out
}

// Certification is the mode's certification.json: the base example's.
func Certification(m Mode) protocol.CertificationFile {
	if m.Base == runner.Name {
		return runner.Certification()
	}
	return protocol.CertificationFile{Command: []string{"weakened-extension", "certify-fixture"}}
}

// Main runs the weakened-extension command.
func Main(args []string, stdin io.Reader, stdout io.Writer) error {
	switch {
	case len(args) == 2 && args[0] == "package-manifest":
		m, ok := Lookup(args[1])
		if !ok {
			return errors.New("unknown mode")
		}
		return printJSON(stdout, Manifest(m))
	case len(args) == 2 && args[0] == "package-certification":
		m, ok := Lookup(args[1])
		if !ok {
			return errors.New("unknown mode")
		}
		return printJSON(stdout, Certification(m))
	case len(args) == 1 && args[0] == "certify-fixture":
		return notes.ServeFixture(stdin, stdout)
	case len(args) == 2 && args[0] == "job":
		return runner.Job(args[1])
	case len(args) != 1:
		return errors.New("usage: weakened-extension MODE")
	}
	m, ok := Lookup(args[0])
	if !ok {
		return errors.New("unknown mode")
	}
	return serve(m, stdin, stdout)
}

func printJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func serve(m Mode, stdin io.Reader, stdout io.Writer) error {
	var server *sdk.Server
	stateDir := func() string {
		if server != nil {
			return server.StateDir()
		}
		return ""
	}
	var ext *sdk.Extension
	if m.Base == runner.Name {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		inner := &runner.Executor{Job: []string{self, "job"}, StateDir: stateDir}
		ext = runner.Build(inner)
		ext.Executors = map[string]extension.Executor{runner.Name: &weakExecutor{Executor: inner, mode: m.Name}}
	} else {
		ext = notes.Build(notes.Options{Release: 2}, nil)
		weakenNotes(m.Name, ext)
	}
	ext.Manifest = Manifest(m)
	if m.Name == "manifest-drift" {
		ext.Manifest.Version += "-drift"
	}
	in := stdin
	switch m.Name {
	case "stdout-noise":
		_, _ = fmt.Fprintln(stdout, "debug: starting the notes extension")
	case "version-blind":
		in = rewriteInitialize(stdin)
	}
	server = sdk.NewServer(ext, in, stdout)
	<-server.Done()
	if m.Name == "no-eof-exit" {
		for { // the defect: never exit
			time.Sleep(time.Hour)
		}
	}
	return nil
}

// rewriteInitialize makes the served extension version-blind: every
// initialize offer is replaced by version 1 before the SDK negotiates.
func rewriteInitialize(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64<<10), protocol.MaxMessageBytes)
		for sc.Scan() {
			line := sc.Bytes()
			var msg map[string]json.RawMessage
			if json.Unmarshal(line, &msg) == nil && string(msg["method"]) == `"initialize"` {
				var p protocol.InitializeParams
				if json.Unmarshal(msg["params"], &p) == nil {
					p.ProtocolVersions = []int{protocol.Version}
					msg["params"], _ = json.Marshal(p)
					line, _ = json.Marshal(msg)
				}
			}
			if _, err := pw.Write(append(append([]byte(nil), line...), '\n')); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()
	return pr
}

func weakenNotes(mode string, ext *sdk.Extension) {
	switch mode {
	case "unstable-push":
		base := ext.Ingress
		ext.Ingress = func(name string) (extension.Ingress, bool) {
			in, ok := base(name)
			if !ok {
				return nil, false
			}
			return unstable{in}, true
		}
	case "route-drift":
		base := ext.Routes[notes.Name]
		ext.Routes[notes.Name] = routeFunc(func(ctx context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
			d, err := base.Match(ctx, in)
			if d.CreateID != "" {
				d.CreateID = domain.ID(string(d.CreateID) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10))
			}
			return d, err
		})
	case "display-link", "display-overlong":
		base := ext.Routes[notes.Name]
		ext.Routes[notes.Name] = routeFunc(func(ctx context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
			d, err := base.Match(ctx, in)
			if d.Display != nil && mode == "display-link" {
				d.Display.Links = []domain.Link{{Label: "Note", URL: "javascript:alert(1)"}}
			} else if d.Display != nil {
				d.Display.Title = strings.Repeat("n", extension.MaxDisplayTitle+1)
			}
			return d, err
		})
	case "operation-drift":
		for i := range ext.Operations {
			if ext.Operations[i].Name == "notes.lookup" {
				base := ext.Operations[i].Handler
				ext.Operations[i].Handler = func(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
					out, err := base(ctx, a, in)
					if err != nil {
						return out, extension.ErrInvalid
					}
					return out, nil
				}
			}
		}
	case "migration-once":
		ext.Migrate = func(ctx context.Context, h *sdk.Host, _, _ int) error {
			if _, _, err := h.Get(ctx, notes.IndexNS, "migrated"); err == nil {
				return errors.New("already migrated")
			}
			return h.Commit(ctx, protocol.KVCommitParams{Puts: []protocol.KVPut{{Namespace: notes.IndexNS, Key: "migrated", Value: []byte("1")}}})
		}
	case "foreign-kv":
		base := ext.Configure
		ext.Configure = func(ctx context.Context, h *sdk.Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
			_ = h.Commit(ctx, protocol.KVCommitParams{Puts: []protocol.KVPut{{Namespace: "slack.projection.x", Key: "k", Value: []byte("x")}}})
			return base(ctx, h, p)
		}
	}
}

type routeFunc func(context.Context, extension.RouteInput) (extension.RouteDecision, error)

func (f routeFunc) Match(ctx context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	return f(ctx, in)
}

type unstable struct{ extension.Ingress }

func (u unstable) Push(ctx context.Context, s extension.Source, d extension.Delivery) (extension.PushResult, error) {
	res, err := u.Ingress.Push(ctx, s, d)
	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	for i := range res.Observations {
		res.Observations[i].Version += "." + stamp
		res.Observations[i].Event.DeliveryID += "." + stamp
	}
	return res, err
}

// weakExecutor is the runner with one defect.
type weakExecutor struct {
	*runner.Executor
	mode string
}

func (w *weakExecutor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	if w.mode == "early-ack" {
		r.PersistRecovery = func(domain.Payload) error { return nil }
	}
	return w.Executor.Run(ctx, r)
}

func (w *weakExecutor) Recover(ctx context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	if w.mode == "replacement-recover" && state.Schema != "" {
		r.Execution.ID = domain.ID(string(r.Execution.ID) + "-replacement")
		return w.Executor.Run(ctx, r)
	}
	return w.Executor.Recover(ctx, r, state)
}

func (w *weakExecutor) Inject(ctx context.Context, id domain.ID, ev domain.Event) error {
	err := w.Executor.Inject(ctx, id, ev)
	if w.mode == "inject-unknown" && errors.Is(err, extension.ErrNotAccepting) {
		return nil
	}
	return err
}
