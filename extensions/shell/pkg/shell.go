// Package shellext serves the shell executor (extensions/shell, unchanged)
// as a package over protocol version 1, through protocol/sdk/inprocsdk
// (#428): the shell executor, with interrupt, inject, stop and forget, in
// the jobs pool. open-conduit serves it as `open-conduit extension-run
// shell`, and the release bundles it.
package shellext

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/open-conduit/open-conduit/extensions/shell"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
)

// Name is the package's registration name: the executor's.
const Name = shell.Name

// Version is this package build's own version.
const Version = "0.1.0"

// base is what the bridge cannot read from the extension's surfaces: the
// executor's own config schema.
func base() protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "Shell",
		Summary: "Runs a binding's shell command as a Work's execution, with the os toolbelt CLI on its PATH.",
		Command: protocol.HostRunCommand(Name),
		Provides: protocol.Provides{Schemas: []protocol.SchemaDecl{{Name: "shell.config", Versions: []int{1}}}}}
}

// New returns the bridge serving a new shell executor.
func New() (*inprocsdk.Bridge, error) { return inprocsdk.New(shell.NewExtension(), base()) }

// Manifest is the package manifest.
func Manifest() protocol.Manifest {
	b, err := New()
	if err != nil {
		panic(err)
	}
	return b.Extension().Manifest
}

// Certification is the package's certification.json: a static fixture.
// The executor case finishes at once; the inject case waits for the
// runner's finish file, then acknowledges everything in its inbox.
func Certification() protocol.CertificationFile {
	script := `while [ ! -e '{tmp}/inject-go' ]; do sleep 0.02; done; for f in "$OS_INBOX"/*.json; do [ -e "$f" ] || continue; d=${f##*-}; d=${d%.json}; : > "$OS_INBOX/consumed/$d"; done`
	run, _ := json.Marshal(map[string]string{"command": "printf certified"})
	inject, _ := json.Marshal(map[string]string{"command": script})
	return protocol.CertificationFile{Fixture: &protocol.Fixture{
		Executor: &protocol.ExecutorCase{Executor: Name, Config: protocol.Payload{Schema: "shell.config.v1", Data: run}},
		Inject:   &protocol.InjectCase{Executor: Name, Config: protocol.Payload{Schema: "shell.config.v1", Data: inject}, FinishFile: "{tmp}/inject-go"},
	}}
}

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run shell` does. Its subcommands package-manifest
// and package-certification print the package files.
func Main(args []string) int {
	b, err := New()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if sdk.PackageCommand(args, b.Extension().Manifest, Certification(), os.Stdout) {
		return 0
	}
	_ = b.Serve()
	return 0
}
