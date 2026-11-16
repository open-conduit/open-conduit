// Command jail-extension is a process sandbox provider for tests: it
// "confines" a launch by running it under a shell that sets JAILED to the
// policy's label, so a test can see the launch went through it and with
// which policy. It confines nothing.
package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

type jail struct{}

func (jail) Plan(_ context.Context, _ extension.ResourceOwner, p extension.SandboxPolicy, _ extension.SandboxNeeds, l extension.SandboxLaunch) (extension.SandboxPlan, error) {
	var policy struct {
		Label string `json:"label"`
	}
	_ = json.Unmarshal(p.Config.Data, &policy)
	argv := append([]string{"/bin/sh", "-c", `JAILED="$0" exec "$@"`, policy.Label}, l.Argv...)
	return extension.SandboxPlan{Argv: argv, Env: l.Env, Signals: extension.SandboxTerm, Summary: "jail " + policy.Label,
		Data: domain.Payload{Schema: "jail.policy.v1", Data: p.Config.Data}}, nil
}

func (jail) Adopt(context.Context, extension.SandboxPlan, int, int) error { return nil }

func main() {
	ext := &sdk.Extension{Manifest: protocol.Manifest{Protocol: []int{protocol.Version}, Name: "jail", Version: "0.1.0", Command: []string{"jail-extension"},
		Provides: protocol.Provides{Sandboxes: []protocol.SandboxDecl{{Type: "jail"}}}},
		Sandboxes: map[string]extension.SandboxProvider{"jail": jail{}}}
	if sdk.PackageCommand(os.Args[1:], ext.Manifest, nil, os.Stdout) {
		return
	}
	out := os.Stdout
	os.Stdout = os.Stderr
	<-sdk.NewServer(ext, os.Stdin, out).Done()
}
