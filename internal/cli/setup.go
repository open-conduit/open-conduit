package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/open-conduit/open-conduit/extension"
)

// Setup (03-05; extension-owned since 04-02): `init <type>` hosts the
// optional extension.Setup of a registered extension. Core supplies only the
// setup host (prompts, opening a URL, the secret store, the run of a helper
// command), merges the result into the local config file and validates it
// offline against the daemon's registrations before saving; a running daemon
// changes only through `config apply`. Nothing here knows any service.

// SetupRequest starts a coding agent with the setup guide (`init --with`).
type SetupRequest struct {
	Agent, ConfigPath, StateDir, Cwd string
}

var (
	scopeIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	refPattern     = regexp.MustCompile(`^(keyring:[a-z][a-z0-9_-]{0,62}|file:[a-z][a-z0-9_-]{0,62}|env:[A-Za-z_][A-Za-z0-9_]*|cmd:[^\r\n]+)$`)
)

func (o *operator) configPath(flag string) string {
	if flag != "" {
		return o.abs(flag)
	}
	if v := o.env.Getenv("OPEN_CONDUIT_CONFIG"); v != "" {
		return o.abs(v)
	}
	return defaultConfigPath(o.env)
}

func toNode(v any) *node {
	b, _ := json.Marshal(v)
	n, _ := parseNode(b)
	return n
}

// MergeSetup merges a setup result into a config document (nil starts a new
// one). A connection whose id already exists is kept; an existing scope with
// the result's id is replaced only with force. Key order and untouched text
// of the existing document are preserved.
func MergeSetup(doc []byte, r extension.SetupResult, force bool) ([]byte, error) {
	var root *node
	if len(bytes.TrimSpace(doc)) == 0 {
		root, _ = parseNode([]byte(`{"version": 1, "settings": {"concurrency": 2}, "connections": [], "scopes": []}`))
	} else {
		var err error
		if root, err = parseNode(doc); err != nil || root.kind != 'o' {
			return nil, fmt.Errorf("existing config is not a JSON object")
		}
	}
	for _, k := range []string{"connections", "scopes"} {
		if root.field(k) == nil {
			_ = root.set([]string{k}, toNode([]any{}))
		}
	}
	id := func(raw json.RawMessage) (*node, string, error) {
		n, err := parseNode(raw)
		if err != nil || n.kind != 'o' {
			return nil, "", fmt.Errorf("setup returned an invalid object")
		}
		var v struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &v) != nil || !scopeIDPattern.MatchString(v.ID) {
			return nil, "", fmt.Errorf("setup returned an object without a valid id")
		}
		return n, v.ID, nil
	}
	conns := root.field("connections")
	for _, raw := range r.Connections {
		n, cid, err := id(raw)
		if err != nil {
			return nil, err
		}
		if _, ok := conns.index(cid); !ok {
			conns.arr = append(conns.arr, n)
		}
	}
	if len(r.Scope) == 0 {
		return root.bytes(), nil
	}
	scope, sid, err := id(r.Scope)
	if err != nil {
		return nil, err
	}
	scopes := root.field("scopes")
	if i, ok := scopes.index(sid); ok {
		if !force {
			return nil, fmt.Errorf("scope %s already exists; choose --scope or pass --force to replace it", sid)
		}
		scopes.arr[i] = scope
	} else {
		scopes.arr = append(scopes.arr, scope)
	}
	return root.bytes(), nil
}

// writePrivate writes data atomically with owner-only permissions.
func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0o600); err == nil {
		if _, err = tmp.Write(data); err == nil {
			err = tmp.Sync()
		}
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// validate checks a candidate offline; problems are printed as pointer,
// code and remedy (like `config validate`).
func (o *operator) validate(path string, data []byte) bool {
	if !json.Valid(data) {
		fmt.Fprintln(o.errOut, "open-conduit: config is not valid JSON")
		return false
	}
	o.warnConfig(data)
	if o.env.ValidateConfig == nil && o.env.ValidateConfigAt == nil {
		return true
	}
	details, err := o.validateConfig(path, data)
	if err != nil {
		fmt.Fprintf(o.errOut, "open-conduit: config could not be validated: %s\n", err)
		return false
	}
	for _, d := range details {
		fmt.Fprintf(o.errOut, "  %s  %s  %s\n", d.Path, d.Code, d.Message)
	}
	return len(details) == 0
}

// warnConfig prints the entrypoint's warnings about the config (a cmd:
// program not on the resolver's PATH, a setting with no effect); they never
// fail it.
func (o *operator) warnConfig(data []byte) {
	if o.env.ConfigWarnings == nil {
		return
	}
	for _, w := range o.env.ConfigWarnings(data) {
		fmt.Fprintf(o.errOut, "open-conduit: warning: %s\n", w)
	}
}

// splitInitFlags removes the host's own flags (--config, --force, --dry-run,
// --with) wherever they appear; everything else belongs to the extension.
func splitInitFlags(args []string) (config, with string, force, dry bool, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
			rest = append(rest, a)
			continue
		}
		switch name {
		case "force", "dry-run":
			b := true
			if hasValue {
				if b, err = strconv.ParseBool(value); err != nil {
					return
				}
			}
			if name == "force" {
				force = b
			} else {
				dry = b
			}
		case "config", "with":
			if !hasValue {
				if i+1 >= len(args) {
					err = fmt.Errorf("--%s needs a value", name)
					return
				}
				i++
				value = args[i]
			}
			if name == "config" {
				config = value
			} else {
				with = value
			}
		default:
			rest = append(rest, a)
		}
	}
	return
}

func (o *operator) setups(configPath string) []extension.Setup {
	out := append([]extension.Setup(nil), o.env.Setups...)
	if o.env.Extensions != nil && o.env.Extensions.Setups != nil {
		out = append(out, o.env.Extensions.Setups(configPath, o.stateDir())...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SetupInfo().Type < out[j].SetupInfo().Type })
	return out
}

func (o *operator) listSetups(w io.Writer, configPath string) {
	fmt.Fprintln(w, "Usage: open-conduit init TYPE [flags] [--config PATH] [--force] [--dry-run]")
	fmt.Fprintln(w, "       open-conduit init --with claude   start Claude Code with the setup guide")
	fmt.Fprintln(w, "\nTypes (from the registered extensions):")
	list := o.setups(configPath)
	if len(list) == 0 {
		fmt.Fprintln(w, "  none: no registered extension offers setup")
	}
	for _, s := range list {
		info := s.SetupInfo()
		fmt.Fprintf(w, "  %-8s %s\n           %s\n", info.Type, info.Summary, info.Usage)
	}
}

func (o *operator) init(args []string) int {
	configFlag, with, force, dry, rest, err := splitInitFlags(args)
	if err != nil {
		return usageError(o.errOut, "invalid init flags")
	}
	path := o.configPath(configFlag)
	if with != "" {
		if o.env.Setup == nil {
			fmt.Fprintln(o.errOut, "open-conduit: init --with is not available in this binary")
			return 1
		}
		return o.env.Setup(SetupRequest{Agent: with, ConfigPath: path, StateDir: o.stateDir(), Cwd: o.env.Cwd})
	}
	if len(rest) == 0 {
		o.listSetups(o.out, path)
		return 0
	}
	if strings.HasPrefix(rest[0], "-") {
		o.listSetups(o.errOut, path)
		return usageError(o.errOut, "init needs a TYPE first, for example init github")
	}
	var setup extension.Setup
	for _, s := range o.setups(path) {
		if s.SetupInfo().Type == rest[0] {
			setup = s
		}
	}
	if setup == nil {
		o.listSetups(o.errOut, path)
		return usageError(o.errOut, fmt.Sprintf("no registered extension offers setup %q", rest[0]))
	}
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return o.emit(nil, fmt.Errorf("config file unreadable"))
	}
	host := &setupHost{o: o, config: existing, configPath: path, dry: dry}
	result, err := setup.Setup(context.Background(), host, rest[1:])
	if errors.Is(err, extension.ErrSetupUsage) {
		return usageError(o.errOut, strings.TrimPrefix(err.Error(), extension.ErrSetupUsage.Error()+": "))
	}
	if err != nil {
		return o.emit(nil, fmt.Errorf("init %s: %w", rest[0], err))
	}
	out, err := MergeSetup(existing, result, force)
	if err != nil {
		return o.emit(nil, err)
	}
	if !o.validate(path, out) {
		fmt.Fprintln(o.errOut, "open-conduit: not saved; nothing changed")
		return 1
	}
	if dry {
		_, _ = o.out.Write(out)
		return 0
	}
	if err = writePrivate(path, out); err != nil {
		return o.emit(nil, fmt.Errorf("could not save the config"))
	}
	fmt.Fprintf(o.out, "Saved %s\n\n", path)
	for _, line := range result.Summary {
		fmt.Fprintf(o.out, "  %s\n", line)
	}
	if len(result.Next) > 0 {
		fmt.Fprintln(o.out, "\nNext:")
		for _, line := range result.Next {
			fmt.Fprintf(o.out, "  %s\n", line)
		}
	}
	if existing != nil {
		fmt.Fprintf(o.out, "A running daemon, even after a restart, keeps its stored configuration until you run: open-conduit config apply %s --generation N (or, if the stored configuration no longer validates, adopts this file when it next starts)\n", path)
	}
	return 0
}

// setupHost is the terminal host of an extension setup.
type setupHost struct {
	o          *operator
	config     []byte
	configPath string
	dry        bool
	reader     *bufio.Reader
}

func (h *setupHost) Printf(format string, args ...any) { fmt.Fprintf(h.o.errOut, format, args...) }
func (h *setupHost) Config() []byte                    { return h.config }
func (h *setupHost) Cwd() string                       { return h.o.env.Cwd }
func (h *setupHost) StateDir() string                  { return h.o.stateDir() }
func (h *setupHost) DryRun() bool                      { return h.dry }

func (h *setupHost) stdin() io.Reader {
	if h.o.env.Stdin != nil {
		return h.o.env.Stdin
	}
	return os.Stdin
}

// readLine reads one answer; a terminal does not echo a secret.
func (h *setupHost) readLine(secret bool) (string, error) {
	in := h.stdin()
	if file, ok := in.(*os.File); ok && secret && term.IsTerminal(file.Fd()) {
		b, err := term.ReadPassword(file.Fd())
		fmt.Fprintln(h.o.errOut)
		return string(b), err
	}
	if h.reader == nil {
		h.reader = bufio.NewReader(io.LimitReader(in, 1<<20))
	}
	line, err := h.reader.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		if errors.Is(err, io.EOF) {
			return "", extension.ErrSetupNoInput
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (h *setupHost) Prompt(question string, p extension.Prompt) (string, error) {
	if p.Default != "" && !p.Secret {
		fmt.Fprintf(h.o.errOut, "%s [%s]: ", question, p.Default)
	} else {
		fmt.Fprintf(h.o.errOut, "%s: ", question)
	}
	answer, err := h.readLine(p.Secret)
	if err != nil {
		if errors.Is(err, extension.ErrSetupNoInput) && p.Default != "" {
			return p.Default, nil
		}
		return "", err
	}
	if p.Secret {
		fmt.Fprintln(h.o.errOut)
	}
	if strings.TrimSpace(answer) == "" {
		return p.Default, nil
	}
	return strings.TrimSpace(answer), nil
}

func (h *setupHost) Choose(question string, options []string) (int, error) {
	if len(options) == 0 {
		return -1, errors.New("nothing to choose from")
	}
	fmt.Fprintln(h.o.errOut, question)
	for i, opt := range options {
		fmt.Fprintf(h.o.errOut, "  %d) %s\n", i+1, opt)
	}
	for tries := 0; tries < 3; tries++ {
		answer, err := h.Prompt("Choose a number", extension.Prompt{Default: "1"})
		if err != nil {
			return -1, err
		}
		if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		fmt.Fprintf(h.o.errOut, "Enter a number from 1 to %d.\n", len(options))
	}
	return -1, errors.New("no valid choice")
}

func (h *setupHost) OpenURL(url string) error {
	fmt.Fprintf(h.o.errOut, "Opening %s\n", url)
	if h.o.env.OpenURL != nil {
		return h.o.env.OpenURL(url)
	}
	name := "xdg-open"
	if goruntime.GOOS == "darwin" {
		name = "open"
	}
	if _, err := exec.LookPath(name); err != nil {
		fmt.Fprintln(h.o.errOut, "No browser opener found: open the URL above yourself.")
		return nil
	}
	return exec.Command(name, url).Run()
}

func (h *setupHost) StoreSecret(name, value string) (string, error) {
	if !scopeIDPattern.MatchString(name) {
		return "", errors.New("secret names match [a-z][a-z0-9_-]*")
	}
	if h.dry {
		return "keyring:" + name, nil
	}
	return h.o.storeSecret(name, value, "", h.configPath)
}

func (h *setupHost) LookPath(name string) (string, bool) {
	if h.o.env.LookPath != nil {
		return h.o.env.LookPath(name)
	}
	p, err := exec.LookPath(name)
	return p, err == nil
}

func (h *setupHost) Run(ctx context.Context, dir, name string, args ...string) error {
	if h.o.env.RunCommand != nil {
		return h.o.env.RunCommand(ctx, dir, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = h.stdin(), h.o.errOut, h.o.errOut
	return cmd.Run()
}

func (o *operator) secretsPath(flag string) string {
	return filepath.Join(filepath.Dir(o.configPath(flag)), "secrets.json")
}

// keyringStore stores a secret in the platform keyring under the
// open-conduit service. The value travels on stdin, never in arguments.
var keyringStore = func(name, value string) error {
	switch goruntime.GOOS {
	case "darwin":
		cmd := exec.Command("/usr/bin/security", "-i")
		cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s open-conduit -a %s -X %s\n", name, hex.EncodeToString([]byte(value))))
		return cmd.Run()
	case "linux":
		cmd := exec.Command("secret-tool", "store", "--label=open-conduit "+name, "service", "open-conduit", "account", name)
		cmd.Stdin = strings.NewReader(value)
		return cmd.Run()
	}
	return errors.New("no platform keyring")
}

// secret stores one secret value read from stdin (a terminal line or piped
// input) and prints the reference to put in secret_refs. The keyring is
// preferred; the owner-only secrets.json beside the config is the reported
// fallback (B05). The value is never echoed, logged or taken as an argument.
func (o *operator) secret(args []string) int {
	if len(args) == 0 {
		return usageError(o.errOut, "expected secret set NAME | secret path")
	}
	f := newFlags(args[0], "store", "config")
	pos, err := f.parse(args[1:])
	if err != nil {
		return usageError(o.errOut, "invalid secret flags")
	}
	switch args[0] {
	case "path":
		fmt.Fprintln(o.out, o.secretsPath(f.get("config")))
		return 0
	case "set":
	default:
		return usageError(o.errOut, "expected secret set NAME | secret path")
	}
	if len(pos) != 1 || !scopeIDPattern.MatchString(pos[0]) {
		return usageError(o.errOut, "secret set takes one NAME matching [a-z][a-z0-9_-]*")
	}
	name, store := pos[0], f.get("store")
	if store != "" && store != "keyring" && store != "file" {
		return usageError(o.errOut, "--store is keyring or file")
	}
	stdin := o.env.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	fmt.Fprintf(o.errOut, "Enter the value for %s, then press Enter (input is read from stdin and never shown again):\n", name)
	var line string
	if file, ok := stdin.(*os.File); ok && term.IsTerminal(file.Fd()) {
		// A terminal does not echo the value.
		b, err := term.ReadPassword(file.Fd())
		fmt.Fprintln(o.errOut)
		if err != nil {
			return o.emit(nil, errors.New("could not read the value"))
		}
		line = string(b)
	} else {
		read, err := bufio.NewReader(io.LimitReader(stdin, 64<<10)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return o.emit(nil, errors.New("could not read the value"))
		}
		line = read
	}
	value := strings.TrimRight(line, "\r\n")
	if value == "" {
		return usageError(o.errOut, "empty value; nothing stored")
	}
	ref, err := o.storeSecret(name, value, store, f.get("config"))
	if err != nil {
		return o.emit(nil, err)
	}
	return o.emit(map[string]string{"reference": ref}, nil)
}

// localConfig implements the file-level config commands; ok is false for
// subcommands that go to the daemon.
func (o *operator) localConfig(args []string) (int, bool) {
	switch args[0] {
	case "path", "show", "check", "get", "set", "unset":
	default:
		return 0, false
	}
	f := newFlags(args[0], "file")
	pos, err := f.parse(args[1:])
	if err != nil {
		return usageError(o.errOut, "invalid config flags"), true
	}
	path := o.configPath(f.get("file"))
	if args[0] == "path" {
		fmt.Fprintln(o.out, path)
		return 0, true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return o.emit(nil, fmt.Errorf("no config at %s; create one with open-conduit init", path)), true
	}
	root, err := parseNode(data)
	if err != nil || root.kind != 'o' {
		return o.emit(nil, fmt.Errorf("config is not a JSON object")), true
	}
	switch args[0] {
	case "show":
		root.redact()
		_, _ = o.out.Write(root.bytes())
		return 0, true
	case "check":
		if !o.validate(path, data) {
			return 1, true
		}
		fmt.Fprintln(o.out, "valid")
		return 0, true
	case "get":
		if len(pos) != 1 {
			return usageError(o.errOut, "expected config get POINTER"), true
		}
		parts, err := pointer(pos[0])
		if err != nil {
			return usageError(o.errOut, err.Error()), true
		}
		n, err := root.lookup(parts)
		if err != nil {
			return o.emit(nil, err), true
		}
		n.redact()
		_, _ = o.out.Write(n.bytes())
		return 0, true
	}
	var parts []string
	switch {
	case args[0] == "set" && len(pos) == 2, args[0] == "unset" && len(pos) == 1:
		if parts, err = pointer(pos[0]); err != nil {
			return usageError(o.errOut, err.Error()), true
		}
	default:
		return usageError(o.errOut, "expected config set POINTER JSON | config unset POINTER"), true
	}
	if args[0] == "set" {
		value, err := parseNode([]byte(pos[1]))
		if err != nil {
			return usageError(o.errOut, "the value must be JSON (quote strings: '\"text\"')"), true
		}
		if len(parts) >= 2 && parts[len(parts)-2] == "secret_refs" && (value.kind != 's' || !refPattern.MatchString(strings.Trim(string(value.scalar), `"`))) {
			return usageError(o.errOut, "a secret_refs value is a reference (keyring:, file:, env:, cmd:); store values with open-conduit secret set"), true
		}
		err = root.set(parts, value)
		if err != nil {
			return o.emit(nil, err), true
		}
	} else if err = root.unset(parts); err != nil {
		return o.emit(nil, err), true
	}
	out := root.bytes()
	if !o.validate(path, out) {
		fmt.Fprintln(o.errOut, "open-conduit: not saved; the config file is unchanged")
		return 1, true
	}
	if err := writePrivate(path, out); err != nil {
		return o.emit(nil, fmt.Errorf("could not save the config")), true
	}
	fmt.Fprintf(o.errOut, "Saved %s. A running daemon, even after a restart, keeps its stored configuration until you run: open-conduit config apply %s --generation N (or, if the stored configuration no longer validates, adopts this file when it next starts)\n", path, path)
	return 0, true
}

// storeSecret stores a value in the platform keyring (preferred) or the
// owner-only secrets.json beside the config, and returns its reference. The
// value is never echoed or logged.
func (o *operator) storeSecret(name, value, store, configFlag string) (string, error) {
	ref := ""
	if store != "file" {
		if err := keyringStore(name, value); err == nil {
			ref = "keyring:" + name
		} else if store == "keyring" {
			return "", errors.New("the platform keyring refused the secret")
		} else {
			fmt.Fprintln(o.errOut, "open-conduit: no usable keyring; falling back to the owner-only secrets file")
		}
	}
	if ref == "" {
		path := o.secretsPath(configFlag)
		values := map[string]string{}
		if data, err := os.ReadFile(path); err == nil {
			if json.Unmarshal(data, &values) != nil {
				return "", fmt.Errorf("%s is not a JSON object of strings; fix it by hand", path)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", errors.New("secrets file unreadable")
		}
		values[name] = value
		names := make([]string, 0, len(values))
		for k := range values {
			names = append(names, k)
		}
		sort.Strings(names)
		var b bytes.Buffer
		b.WriteString("{\n")
		for i, k := range names {
			kb, _ := marshal(k)
			vb, _ := marshal(values[k])
			fmt.Fprintf(&b, "  %s: %s", kb, vb)
			if i < len(names)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString("}\n")
		if err := writePrivate(path, b.Bytes()); err != nil {
			return "", errors.New("could not save the secrets file")
		}
		ref = "file:" + name
		fmt.Fprintf(o.errOut, "Stored in %s (0600)\n", path)
	}
	return ref, nil
}
