package extproc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

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
