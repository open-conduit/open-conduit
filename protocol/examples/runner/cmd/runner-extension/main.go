// Command runner-extension serves the example external executor over
// protocol version 1 on stdin/stdout, or runs one detached job
// (runner-extension job <run dir>) (07-02).
package main

import (
	"fmt"
	"os"
	"sync/atomic"

	"github.com/alsiola/open-conduit/protocol/examples/runner"
	"github.com/alsiola/open-conduit/protocol/sdk"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "job" {
		if err := runner.Job(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out := os.Stdout
	os.Stdout = os.Stderr
	var server atomic.Pointer[sdk.Server]
	e := &runner.Executor{Job: []string{self, "job"}, StateDir: func() string {
		if s := server.Load(); s != nil {
			return s.StateDir()
		}
		return ""
	}}
	s := sdk.NewServer(runner.Build(e), os.Stdin, out)
	server.Store(s)
	<-s.Done()
}
