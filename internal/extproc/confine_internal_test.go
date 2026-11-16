package extproc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

type fakeSandbox struct{ needs extension.SandboxNeeds }

func (f *fakeSandbox) Plan(_ context.Context, n extension.SandboxNeeds, l extension.SandboxLaunch) (extension.SandboxPlan, error) {
	f.needs = n
	return extension.SandboxPlan{Argv: append([]string{"sandboxed"}, l.Argv...), Digest: "d1"}, nil
}
func (f *fakeSandbox) Adopt(context.Context, int) error { return nil }

type fakeContainer struct{}

func (fakeContainer) Launch(_ context.Context, _ extension.SandboxNeeds, l extension.SandboxLaunch) (extension.SandboxPlan, error) {
	return extension.SandboxPlan{Argv: append([]string{"contained"}, l.Argv...)}, nil
}
func (fakeContainer) Adopt(context.Context, bool) (extension.ContainerExit, error) {
	return extension.ContainerExit{Exited: true, ExitCode: 3}, nil
}

// A packaged executor confines a run through the brokers the host bound to
// it (#428): sandbox.plan and container.launch reach them under the run's
// ID, and a run without one, or a run this process doesn't serve, is
// refused.
func TestRunBrokersConfineThroughTheHost(t *testing.T) {
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "acme", Version: "1.0.0", Command: []string{"./acme"},
		Provides: protocol.Provides{Executors: []protocol.ExecutorDecl{{Name: "acme", Capabilities: protocol.Capabilities{Toolbelt: "none", Sandbox: true, Container: true}}}}}
	s, err := NewService(m, Deps{Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	sandbox := &fakeSandbox{}
	s.openRun("boxed", &run{sandbox: sandbox})
	s.openRun("contained", &run{container: fakeContainer{}})
	s.openRun("plain", &run{})
	call := func(method string, p any) (any, error) {
		raw, _ := json.Marshal(p)
		return s.Handle(context.Background(), method, raw)
	}
	launch := protocol.LaunchParams{RunID: "boxed", Needs: protocol.SandboxNeeds{Write: []string{"/w"}}, Launch: protocol.SandboxLaunch{Argv: []string{"sh"}}}
	out, err := call("sandbox.plan", launch)
	if err != nil || out.(protocol.SandboxPlan).Argv[0] != "sandboxed" || out.(protocol.SandboxPlan).Digest != "d1" || sandbox.needs.Write[0] != "/w" {
		t.Fatalf("sandbox.plan: %+v %v", out, err)
	}
	launch.RunID = "contained"
	if out, err := call("container.launch", launch); err != nil || out.(protocol.SandboxPlan).Argv[0] != "contained" {
		t.Fatalf("container.launch: %+v %v", out, err)
	}
	if out, err := call("container.adopt", protocol.ContainerAdoptParams{RunID: "contained"}); err != nil || !out.(protocol.ContainerExit).Exited || out.(protocol.ContainerExit).ExitCode != 3 {
		t.Fatalf("container.adopt: %+v %v", out, err)
	}
	launch.RunID = "plain"
	if _, err := call("sandbox.plan", launch); protocol.ErrorCode(err) != protocol.CodeUnsupported {
		t.Fatalf("sandbox.plan for a run with no sandbox: %v", err)
	}
	if _, err := call("container.launch", launch); protocol.ErrorCode(err) != protocol.CodeUnsupported {
		t.Fatalf("container.launch for a run with no container: %v", err)
	}
	launch.RunID = "other"
	if _, err := call("sandbox.plan", launch); protocol.ErrorCode(err) != protocol.CodeConflict {
		t.Fatalf("sandbox.plan for a run not served: %v", err)
	}
}

// A package whose entry type declares a sandbox has its own processes for
// an entry planned through the host (sandbox.entry_plan), in scopes that
// use it only; a package that declares none can't ask.
func TestEntrySandboxThroughTheHost(t *testing.T) {
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "bridge", Version: "1.0.0", Command: []string{"./bridge"},
		Provides: protocol.Provides{Extensions: []protocol.ExtensionDecl{{Type: "bridge-stdio", Sandbox: protocol.EntrySandboxRequired}}}}
	var asked struct {
		scope domain.ID
		entry string
		argv  []string
	}
	deps := Deps{Now: time.Now, Scopes: func() []domain.ID { return []domain.ID{"org"} },
		EntrySandbox: func(_ context.Context, scope domain.ID, entry string, _ extension.SandboxNeeds, l extension.SandboxLaunch) (extension.SandboxPlan, string, error) {
			asked.scope, asked.entry, asked.argv = scope, entry, l.Argv
			return extension.SandboxPlan{Argv: append([]string{"jailed"}, l.Argv...), Digest: "d1"}, "/state/entries/bridge/runs/crm-1", nil
		}}
	s, err := NewService(m, deps)
	if err != nil {
		t.Fatal(err)
	}
	call := func(s *Service, p protocol.EntrySandboxParams) (any, error) {
		raw, _ := json.Marshal(p)
		return s.Handle(context.Background(), "sandbox.entry_plan", raw)
	}
	out, err := call(s, protocol.EntrySandboxParams{Scope: "org", Entry: "crm", Launch: protocol.SandboxLaunch{Argv: []string{"server"}}})
	if err != nil {
		t.Fatal(err)
	}
	if r := out.(protocol.EntrySandboxResult); r.Plan.Argv[0] != "jailed" || r.Dir != "/state/entries/bridge/runs/crm-1" || asked.scope != "org" || asked.entry != "crm" || asked.argv[0] != "server" {
		t.Fatalf("sandbox.entry_plan = %+v, asked %+v", r, asked)
	}
	if _, err := call(s, protocol.EntrySandboxParams{Scope: "elsewhere", Entry: "crm", Launch: protocol.SandboxLaunch{Argv: []string{"server"}}}); protocol.ErrorCode(err) != protocol.CodeUnauthorized {
		t.Fatalf("a scope that doesn't use the package: %v", err)
	}
	m.Provides.Extensions[0].Sandbox = ""
	plain, err := NewService(m, deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(plain, protocol.EntrySandboxParams{Scope: "org", Entry: "crm", Launch: protocol.SandboxLaunch{Argv: []string{"server"}}}); err == nil {
		t.Fatal("a package with no entry sandbox planned one")
	}
}
