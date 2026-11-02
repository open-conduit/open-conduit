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
// toolbelt needs it. With cached, a pass in the last ten minutes stands.
func (p *Provider) Health(ctx context.Context, s Settings, dir string, cached bool) Health {
	key := s.Endpoint() + "\x00" + s.HostKey + "\x00" + dir
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
	return h
}
