// Command goplugin-ext is the prototype extension under go-plugin (gRPC).
package main

import (
	"github.com/alsiola/open-conduit/prototypes/transport/goplugin"
	"github.com/alsiola/open-conduit/prototypes/transport/scenario"
)

func main() { goplugin.Serve("goplugin-ext", scenario.Extension) }
