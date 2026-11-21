package gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// ProbeOptions are what the preflight checks from inside a container on a
// preflight internal network: Alias must resolve (the gateway is there),
// Outside must not (no DNS out), and none of Unreachable (host:port, the
// host's listener through host.docker.internal and the bridge addresses)
// may be reached.
type ProbeOptions struct {
	Alias, Outside string
	Unreachable    []string
	Timeout        time.Duration
}

// Probe runs the checks and writes one line for each, "ok <check>" or
// "fail <check>: <why>". It reports whether all passed.
func Probe(ctx context.Context, o ProbeOptions, out io.Writer) bool {
	if o.Timeout == 0 {
		o.Timeout = 3 * time.Second
	}
	ok := true
	report := func(check string, err error) {
		if err != nil {
			ok = false
			fmt.Fprintf(out, "fail %s: %v\n", check, err)
			return
		}
		fmt.Fprintf(out, "ok %s\n", check)
	}
	var r net.Resolver
	lookup := func(name string) error {
		c, cancel := context.WithTimeout(ctx, o.Timeout)
		defer cancel()
		_, err := r.LookupHost(c, name)
		return err
	}
	if o.Alias != "" {
		report("resolves "+o.Alias, lookup(o.Alias))
	}
	if o.Outside != "" {
		err := lookup(o.Outside)
		if err == nil {
			err = fmt.Errorf("%s resolved: DNS reaches outside the network", o.Outside)
		} else {
			err = nil
		}
		report("no outside DNS", err)
	}
	for _, addr := range o.Unreachable {
		d := net.Dialer{Timeout: o.Timeout}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			c.Close()
			err = fmt.Errorf("%s answered", addr)
		} else {
			err = nil
		}
		report("can't reach "+addr, err)
	}
	return ok
}

// ParseProbe reads `probe` flags: --alias, --outside and --unreachable
// (comma-separated, may repeat).
func ParseProbe(args []string) (ProbeOptions, error) {
	var o ProbeOptions
	for i := 0; i+1 < len(args); i += 2 {
		switch args[i] {
		case "--alias":
			o.Alias = args[i+1]
		case "--outside":
			o.Outside = args[i+1]
		case "--unreachable":
			for _, a := range strings.Split(args[i+1], ",") {
				if a = strings.TrimSpace(a); a != "" {
					o.Unreachable = append(o.Unreachable, a)
				}
			}
		default:
			return ProbeOptions{}, fmt.Errorf("unknown flag %s", args[i])
		}
	}
	if len(args)%2 != 0 {
		return ProbeOptions{}, fmt.Errorf("%s needs a value", args[len(args)-1])
	}
	return o, nil
}
