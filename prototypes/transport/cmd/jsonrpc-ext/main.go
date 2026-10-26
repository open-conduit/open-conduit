// Command jsonrpc-ext is the prototype extension over JSON-RPC 2.0 on stdio.
package main

import (
	"github.com/alsiola/open-conduit/prototypes/transport/jsonrpc"
	"github.com/alsiola/open-conduit/prototypes/transport/scenario"
)

func main() { jsonrpc.Serve("jsonrpc-ext", scenario.Extension) }
