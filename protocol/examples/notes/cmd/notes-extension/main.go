// Command notes-extension serves the example external extension over
// protocol version 1 on stdin/stdout (07-02). The package-manifest,
// package-certification and certify-fixture subcommands serve packaging
// and certification (07-03).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/open-conduit/open-conduit/protocol/examples/notes"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

func main() {
	if sdk.PackageCommand(os.Args[1:], notes.Manifest(notes.Options{Release: 2}), notes.Certification(), os.Stdout) {
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "certify-fixture" {
		if err := notes.ServeFixture(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	release := flag.Int("release", 2, "release to serve (1, 2 or 3)")
	stable := flag.Bool("stable-identity", false, "omit the payload schema from observation identities (negative test)")
	fixture := flag.Bool("fixture", false, "serve the shared toolbelt suite's fixture profile")
	flag.Parse()
	protocolOut := os.Stdout
	os.Stdout = os.Stderr
	var server atomic.Pointer[sdk.Server]
	o := notes.Options{Release: *release, StableIdentity: *stable, Fixture: *fixture, StateDir: func() string {
		if s := server.Load(); s != nil {
			return s.StateDir()
		}
		return ""
	}}
	ext := notes.Build(o, func(f notes.Fault) {
		switch f.Mode {
		case "exit":
			os.Exit(3)
		case "partial":
			// Half a response line, then death: the host must see a
			// transport failure, never a result.
			_, _ = protocolOut.WriteString(`{"jsonrpc":"2.0","id":`)
			os.Exit(3)
		case "hang":
			time.Sleep(time.Hour)
		}
	})
	notes.SetReenter(func() ([]byte, string, error) {
		return server.Load().Host().Get(context.Background(), notes.IndexNS, "reentrant")
	})
	s := sdk.NewServer(ext, os.Stdin, protocolOut)
	server.Store(s)
	<-s.Done()
}
