// Command open-conduit-gateway is the program in the egress gateway's
// image: `serve` is the allowlisting proxy and the toolbelt forward one
// gateway runs for a launch (its flags are a contract any container
// provider may run against), and `probe` is the Docker provider's doctor
// preflight from inside a container (see the gateway package).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/open-conduit/open-conduit/gateway"
)

var version = "dev"
var commit = "unknown"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: open-conduit-gateway serve|probe|version [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "--version":
		fmt.Printf("open-conduit-gateway %s (%s)\n", version, commit)
	case "serve":
		o, err := gateway.Parse(os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "open-conduit-gateway:", err)
			os.Exit(2)
		}
		g, err := gateway.Start(o)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open-conduit-gateway:", err)
			os.Exit(1)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		<-ctx.Done()
		stop()
		g.Close()
	case "probe":
		o, err := gateway.ParseProbe(os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "open-conduit-gateway:", err)
			os.Exit(2)
		}
		if !gateway.Probe(context.Background(), o, os.Stdout) {
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "open-conduit-gateway: unknown command", os.Args[1])
		os.Exit(2)
	}
}
