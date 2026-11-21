package ssh

import (
	"context"
	"fmt"
	"strings"
)

// Health is what doctor reports for one remote: whether a binding can run
// on it, and why not.
type Health struct {
	// Status is ok or fail; Code names the first problem.
	Status, Code, Detail string
	Items                []string
}

// Health checks the remote is reachable with its pinned host key, has
// what a run needs (sh, setsid, ps, tar, find and env), and can write its
// run records and dir, a host path mirrored there. The toolbelt's os on
// the remote's PATH is reported, not required: only a binding with a
// toolbelt needs it. A remote behind the gateway must also fail to connect
// out directly, or its allowlist would not be enforced. With cached, a pass in the last ten minutes stands.
func (p *Provider) Health(ctx context.Context, s Settings, dir string, cached bool) Health {
	key := s.Endpoint() + "\x00" + s.HostKey + "\x00" + dir + "\x00" + s.Network
	if cached {
		p.mu.Lock()
		at, ok := p.health[key]
		p.mu.Unlock()
		if ok && p.o.Now().Sub(at) < healthTTL {
			return Health{Status: "ok", Code: "ok"}
		}
	}
	h := p.check(ctx, s, dir)
	if h.Status == "ok" {
		p.mu.Lock()
		p.health[key] = p.o.Now()
		p.mu.Unlock()
	}
	return h
}

func (p *Provider) check(ctx context.Context, s Settings, dir string) Health {
	var args []string
	if dir != "" {
		args = append(args, dir)
	}
	out, err := p.ssh(ctx, s, nil, scriptHealth, args...)
	if err != nil {
		return Health{Status: "fail", Code: "remote_unreachable", Detail: fmt.Sprintf("can't reach %s with its pinned host key: %v", s.Endpoint(), err)}
	}
	h := Health{Status: "ok", Code: "ok"}
	system := "unknown system"
	var missing, unwritable []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fact, rest, _ := strings.Cut(line, " ")
		switch fact {
		case "missing":
			missing = append(missing, rest)
		case "unwritable":
			unwritable = append(unwritable, rest)
		case "no-os":
			h.Items = append(h.Items, s.Endpoint()+" has no os on its PATH: a binding with a toolbelt can't reach it there")
		case "uname":
			system = rest
		}
	}
	h.Detail = fmt.Sprintf("%s (%s), host key pinned", s.Endpoint(), system)
	switch {
	case len(missing) > 0:
		h.Status, h.Code = "fail", "remote_missing_tool"
		h.Detail = fmt.Sprintf("%s lacks %s", s.Endpoint(), strings.Join(missing, ", "))
	case len(unwritable) > 0:
		h.Status, h.Code = "fail", "remote_unwritable"
		h.Detail = fmt.Sprintf("%s can't write %s: the host's paths are mirrored at the same paths there", s.Endpoint(), strings.Join(unwritable, ", "))
	}
	if h.Status == "ok" && s.Network == NetworkGateway {
		p.checkEgress(ctx, s, &h)
	}
	return h
}

// checkEgress fails closed: a remote that reaches out directly, or whose
// way out can't be checked, doesn't run a binding that says its allowlist
// is enforced.
func (p *Provider) checkEgress(ctx context.Context, s Settings, h *Health) {
	out, err := p.ssh(ctx, s, nil, scriptEgress, p.o.EgressProbe...)
	if err != nil {
		h.Status, h.Code, h.Detail = "fail", "remote_egress_unchecked", fmt.Sprintf("can't check %s's way out: %v", s.Endpoint(), err)
		return
	}
	var open []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fact, rest, _ := strings.Cut(line, " ")
		switch fact {
		case "unchecked":
			why := map[string]string{
				"no-nc":   "has no nc to check it can't connect out",
				"control": "has an nc that can't reach its own sshd, so a refused connection proves nothing",
				"ipv6":    "has a global IPv6 address and an nc without -6, so its IPv6 way out can't be checked",
			}[rest]
			if why == "" {
				why = "can't check its way out"
			}
			h.Status, h.Code, h.Detail = "fail", "remote_egress_unchecked", fmt.Sprintf(`%s %s: install OpenBSD netcat or ncat, or set "network": "host"`, s.Endpoint(), why)
			return
		case "reaches":
			open = append(open, rest)
		}
	}
	if len(open) > 0 {
		h.Status, h.Code = "fail", "remote_egress_open"
		h.Detail = fmt.Sprintf(`%s connects directly to %s, so the gateway isn't its only way out: firewall its outbound traffic, or set "network": "host"`, s.Endpoint(), strings.Join(open, ", "))
	}
}
