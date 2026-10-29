package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// Probe limits: each run has a short timeout and its output a cap.
const (
	ProbeTimeout   = 20 * time.Second
	maxProbeOutput = 32 << 10
	maxListEntries = 300
)

// ProbeEvent is a setup.probe Event's data: what the agent ran, why, and
// its safe result (Redacted when it looked like it held a secret).
type ProbeEvent struct {
	Probe    string            `json:"probe"`
	Args     map[string]string `json:"args,omitempty"`
	Command  string            `json:"command,omitempty"`
	Why      string            `json:"why,omitempty"`
	Result   json.RawMessage   `json:"result,omitempty"`
	Error    string            `json:"error,omitempty"`
	Redacted bool              `json:"redacted,omitempty"`
}

// Probes are the setup extension's own probes: `setup.fs.list` and
// `setup.tools`. Home and LookPath are the system's unless set (tests).
type Probes struct {
	Home     string
	LookPath func(string) (string, error)
}

var _ extension.SetupProbes = Probes{}

func (p Probes) home() (string, error) {
	if p.Home != "" {
		return p.Home, nil
	}
	return os.UserHomeDir()
}

func (p Probes) SetupProbes() []extension.SetupProbe {
	return []extension.SetupProbe{
		{Name: "fs.list", Summary: "a directory's entries under the operator's home, names and kinds only, one or two levels; git checkouts list their remotes' URLs without credentials",
			Args: []extension.ProbeArg{{Name: "dir", Summary: "a directory under the home directory (~/code or an absolute path)", Required: true}, {Name: "depth", Summary: "1 (default) or 2"}},
			Run:  p.list},
		{Name: "tools", Summary: "whether gh, git, ngrok and cloudflared are on the daemon's PATH, where, and whether each is a symlink or a script",
			Run: p.tools},
	}
}

// Entry is one directory entry fs.list returns.
type Entry struct {
	Path    string   `json:"path"`
	Kind    string   `json:"kind"`
	Remotes []string `json:"remotes,omitempty"`
}

func (p Probes) list(_ context.Context, args map[string]string) (extension.ProbeResult, error) {
	home, err := p.home()
	if err != nil {
		return extension.ProbeResult{}, fmt.Errorf("the home directory is unknown")
	}
	dir := args["dir"]
	if rest, ok := strings.CutPrefix(dir, "~"); ok {
		dir = filepath.Join(home, rest)
	}
	depth := 1
	if d := args["depth"]; d != "" {
		if depth, err = strconv.Atoi(d); err != nil || depth < 1 || depth > 2 {
			return extension.ProbeResult{}, fmt.Errorf("%w: depth is 1 or 2", extension.ErrProbeInput)
		}
	}
	if !filepath.IsAbs(dir) {
		return extension.ProbeResult{}, fmt.Errorf("%w: dir is ~/… or an absolute path", extension.ErrProbeInput)
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return extension.ProbeResult{}, fmt.Errorf("the home directory is unknown")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return extension.ProbeResult{}, fmt.Errorf("%w: no such directory", extension.ErrProbeInput)
	}
	if real != realHome && !strings.HasPrefix(real, realHome+string(filepath.Separator)) {
		return extension.ProbeResult{}, fmt.Errorf("%w: only directories under the home directory can be listed", extension.ErrProbeInput)
	}
	out := struct {
		Dir       string  `json:"dir"`
		Entries   []Entry `json:"entries"`
		Hidden    int     `json:"hidden"`
		Truncated bool    `json:"truncated,omitempty"`
	}{Dir: dir, Entries: []Entry{}}
	var walk func(abs, rel string, level int)
	walk = func(abs, rel string, level int) {
		items, err := os.ReadDir(abs)
		if err != nil {
			return
		}
		for _, it := range items {
			if strings.HasPrefix(it.Name(), ".") {
				out.Hidden++
				continue
			}
			if len(out.Entries) == maxListEntries {
				out.Truncated = true
				return
			}
			e := Entry{Path: filepath.Join(rel, it.Name()), Kind: "file"}
			full := filepath.Join(abs, it.Name())
			switch {
			case it.Type()&os.ModeSymlink != 0:
				e.Kind = "symlink"
			case it.IsDir():
				e.Kind = "dir"
				if remotes, ok := gitRemotes(full); ok {
					e.Kind, e.Remotes = "git", remotes
				}
			}
			out.Entries = append(out.Entries, e)
			if e.Kind == "dir" && level < depth {
				walk(full, e.Path, level+1)
			}
		}
	}
	walk(real, "", 1)
	return extension.ProbeResult{Command: "ls " + dir, Output: out}, nil
}

// gitRemotes reads a checkout's remotes from .git/config, keeping only the
// host and path of each URL: user info (a token in an https remote) never
// leaves.
func gitRemotes(dir string) ([]string, bool) {
	f, err := os.Open(filepath.Join(dir, ".git", "config"))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	remotes := []string{}
	name := ""
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, "[") {
			name = ""
			if r, ok := strings.CutPrefix(line, `[remote "`); ok {
				name, _, _ = strings.Cut(r, `"`)
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if name == "" || !ok || strings.TrimSpace(key) != "url" {
			continue
		}
		remotes = append(remotes, name+" "+safeRemote(strings.TrimSpace(value)))
	}
	sort.Strings(remotes)
	return remotes, true
}

func safeRemote(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host + u.Path
	}
	// scp-like git@host:owner/repo
	if at := strings.LastIndex(raw, "@"); at >= 0 && strings.Contains(raw[at:], ":") {
		return raw[at+1:]
	}
	return raw
}

func (p Probes) tools(context.Context, map[string]string) (extension.ProbeResult, error) {
	look := p.LookPath
	if look == nil {
		look = exec.LookPath
	}
	type tool struct {
		Name    string `json:"name"`
		Found   bool   `json:"found"`
		Path    string `json:"path,omitempty"`
		Symlink bool   `json:"symlink,omitempty"`
		Script  bool   `json:"script,omitempty"`
	}
	out := []tool{}
	for _, name := range []string{"gh", "git", "ngrok", "cloudflared"} {
		t := tool{Name: name}
		if path, err := look(name); err == nil {
			t.Found, t.Path = true, path
			if fi, err := os.Lstat(path); err == nil {
				t.Symlink = fi.Mode()&os.ModeSymlink != 0
			}
			if f, err := os.Open(path); err == nil {
				head := make([]byte, 2)
				n, _ := f.Read(head)
				t.Script = n == 2 && string(head) == "#!"
				f.Close()
			}
		}
		out = append(out, t)
	}
	return extension.ProbeResult{Command: "command -v gh git ngrok cloudflared", Output: map[string]any{"tools": out}}, nil
}

// probes is every probe a session may run, by full name: the extension's
// own and those the host collects from other extensions and executors.
func (t *Tools) probes() map[string]extension.SetupProbe {
	out := map[string]extension.SetupProbe{}
	for _, p := range t.o.OwnProbes.SetupProbes() {
		out[Type+"."+p.Name] = p
	}
	if t.o.Probes != nil {
		for name, p := range t.o.Probes() {
			out[name] = p
		}
	}
	return out
}

// probe runs a named probe for the session and records it as setup.probe.
// Output that looks like it holds a secret is replaced with a redacted
// marker before anything sees it.
func (t *Tools) probe(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
	var input struct {
		Probe string            `json:"probe"`
		Args  map[string]string `json:"args,omitempty"`
		Why   string            `json:"why"`
	}
	if err := decode(in.Data, &input, "{probe, args?, why}"); err != nil {
		return domain.Payload{}, err
	}
	why, err := text(input.Why, maxContext, "why")
	if err != nil {
		return domain.Payload{}, err
	}
	all := t.probes()
	p, ok := all[input.Probe]
	if !ok {
		names := make([]string, 0, len(all))
		for name := range all {
			names = append(names, name)
		}
		sort.Strings(names)
		return domain.Payload{}, refuse("unknown_probe", "no probe %q; the probes are: %s", input.Probe, strings.Join(names, ", "))
	}
	if err = probeArgs(p, input.Args); err != nil {
		return domain.Payload{}, err
	}
	if err = scanText(append(values(input.Args), why)...); err != nil {
		return domain.Payload{}, err
	}
	if err = t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		_, err := t.session(tx, a)
		return err
	}); err != nil {
		return domain.Payload{}, err
	}
	cx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	res, runErr := p.Run(cx, input.Args)
	cancel()
	ev := ProbeEvent{Probe: input.Probe, Args: input.Args, Command: res.Command, Why: why}
	out := map[string]any{"probe": input.Probe}
	switch {
	case runErr != nil:
		ev.Error = probeError(runErr)
		out["error"] = ev.Error
	case looksLikeSecret(res.Output) || looksLikeSecret(res.Command):
		ev.Redacted, ev.Command = true, ""
		out["redacted"] = true
		out["note"] = "the output looked like it held a secret, so it was withheld"
	default:
		b, err := json.Marshal(res.Output)
		if err != nil || len(b) > maxProbeOutput {
			ev.Error = "the output was too large; narrow the probe"
			out["error"] = ev.Error
			break
		}
		ev.Result = b
		out["result"] = json.RawMessage(b)
	}
	req := requestID(ctx, a)
	if err = t.o.Store.Transact(ctx, storage.Guard{Now: t.o.Now()}, func(tx storage.Tx) error {
		return t.accept(tx, a, TypeProbe, SchemaProbe, req, ev)
	}); err != nil {
		return domain.Payload{}, err
	}
	return payload(SchemaTool, out), nil
}

func probeArgs(p extension.SetupProbe, args map[string]string) error {
	known := map[string]bool{}
	for _, a := range p.Args {
		known[a.Name] = true
		if a.Required && strings.TrimSpace(args[a.Name]) == "" {
			return refuse("invalid_input", "%s needs %s (%s)", p.Name, a.Name, a.Summary)
		}
	}
	for name, v := range args {
		if !known[name] {
			return refuse("invalid_input", "%s takes no argument %q", p.Name, name)
		}
		if len(v) > 512 || strings.ContainsAny(v, "\x00\r\n") {
			return refuse("invalid_input", "argument %s must be one line of at most 512 bytes", name)
		}
	}
	return nil
}

func values(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// probeError is a probe's error for the agent: its own fixed reason, or a
// timeout.
func probeError(err error) string {
	if strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		return "the probe timed out"
	}
	msg := err.Error()
	if secretShapes.MatchString(msg) {
		return "the probe failed"
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}
