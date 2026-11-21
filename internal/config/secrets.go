package config

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/extension"
)

// KeyringService is the only keyring service Open Conduit reads.
const KeyringService = "open-conduit"

// maxSecretOutput bounds command and keyring helper output.
const maxSecretOutput = 64 << 10

// ErrSecret wraps every resolution failure. Errors carry only a safe code and
// secret name, never reference text, commands, output or provider errors.
var ErrSecret = errors.New("secret unavailable")

type secretError struct{ code, name string }

func (e *secretError) Error() string {
	if e.name != "" {
		return ErrSecret.Error() + ": " + e.code + " (" + e.name + ")"
	}
	return ErrSecret.Error() + ": " + e.code
}
func (e *secretError) Unwrap() error { return ErrSecret }

// SecretCode returns the safe failure code of a resolution error.
func SecretCode(err error) string {
	var se *secretError
	if errors.As(err, &se) {
		return se.code
	}
	return ""
}

type Keyring interface {
	Get(service, name string) (string, error)
}

// Resolver resolves references lazily for one consumer. It caches nothing
// for the built-in forms; a provider's answers are held in Cache, when set.
type Resolver struct {
	Keyring        Keyring // nil uses the platform keyring helper
	SecretsFile    string  // owner-only secrets.json for file: references
	LookupEnv      func(string) (string, bool)
	CommandTimeout time.Duration // default 10s; also bounds a provider's answer
	// Providers finds the running package that serves a provider scheme
	// (manifest secret_schemes), with its name. A scheme it doesn't find
	// fails as provider_unavailable: there is no fallback.
	Providers func(scheme string) (p extension.SecretProvider, pkg string, ok bool)
	// Observe, when set, is told of each provider-backed resolution, by
	// reference and outcome, never value.
	Observe func(context.Context, SecretResolution)
	// Cache, when set, holds provider-served answers for a bounded
	// lifetime, shared by every resolver For returns (SecretCache).
	Cache *SecretCache

	consumer string
	fresh    bool
}

// SecretResolution is one provider-backed resolution as it is recorded:
// the secret's name in its slot ("" when the caller has none), the
// reference (which a record carries only as a digest, since reference text
// is sensitive), the package that served it, the package it was resolved
// for ("" for the host itself: an attempt's start, doctor) and the safe
// failure code ("" when it resolved).
type SecretResolution struct {
	Name, Reference, Scheme, Provider, Consumer, Code string
	Duration                                          time.Duration
}

// For returns a resolver that resolves for the package consumer, which
// may never be served by a scheme it provides itself (provider_cycle).
func (r *Resolver) For(consumer string) *Resolver {
	c := *r
	c.consumer = consumer
	return &c
}

// Fresh returns a resolver that asks providers now rather than answering
// from Cache, for a check of whether a reference resolves now (doctor,
// GET /v1/secrets). It leaves the cache as it was.
func (r *Resolver) Fresh() *Resolver {
	c := *r
	c.fresh = true
	return &c
}

func (r *Resolver) lookup(k string) (string, bool) {
	if r.LookupEnv != nil {
		return r.LookupEnv(k)
	}
	return os.LookupEnv(k)
}
func (r *Resolver) Resolve(ctx context.Context, ref string) (string, error) {
	return r.ResolveNamed(ctx, "", ref)
}

// ResolveNamed resolves ref for the secret slot name, which a
// provider-backed resolution's record names.
func (r *Resolver) ResolveNamed(ctx context.Context, name, ref string) (string, error) {
	if !ValidSecretRef(ref) {
		return "", &secretError{code: "invalid_reference"}
	}
	scheme, rest, _ := strings.Cut(ref, ":")
	var v string
	switch scheme {
	case "env":
		v, _ = r.lookup(rest)
		if v == "" {
			return "", &secretError{code: "env_missing"}
		}
	case StoredScheme:
		var code string
		if v, code = r.stored(ctx, rest); code != "" {
			return "", &secretError{code: code}
		}
	case "file":
		var code string
		if v, code = r.fromFile(rest); code != "" {
			return "", &secretError{code: code}
		}
	case "keyring":
		var err error
		if v, err = r.fromKeyring(ctx, rest); err != nil {
			return "", &secretError{code: "keyring_unavailable"}
		}
	case "cmd":
		out, code := r.run(ctx, r.baseEnv(), "sh", "-c", rest)
		if code != "" {
			return "", &secretError{code: code}
		}
		v = out
	default:
		return r.provided(ctx, name, ref, scheme, rest)
	}
	if v == "" {
		return "", &secretError{code: "empty"}
	}
	return v, nil
}

// stored reads the value the daemon's secret store holds under name: the
// secrets file's if it has the name, else the keyring's. SecretStore keeps
// the file's copy current whenever both hold a name, so the order can't
// serve a stale value. A secrets file that exists but can't be read fails
// rather than falling through to a keyring value it might replace.
func (r *Resolver) stored(ctx context.Context, name string) (string, string) {
	if r.SecretsFile != "" {
		_, err := os.Lstat(r.SecretsFile)
		switch {
		case err == nil:
			if v, code := r.fromFile(name); code != "file_missing" {
				return v, code
			}
		case !errors.Is(err, fs.ErrNotExist):
			return "", "file_unavailable"
		}
	}
	v, err := r.fromKeyring(ctx, name)
	if err != nil {
		return "", "stored_missing"
	}
	return v, ""
}

// fromFile is the value the secrets file holds under name, or a safe code.
func (r *Resolver) fromFile(name string) (string, string) {
	data, err := ReadFile(r.SecretsFile)
	if err != nil || r.SecretsFile == "" {
		return "", "file_unavailable"
	}
	root, diags := decode(data)
	if root == nil || len(diags) > 0 || root.kind != kObject {
		return "", "file_invalid"
	}
	n := root.field(name)
	if n == nil {
		return "", "file_missing"
	}
	if n.kind != kString {
		return "", "file_invalid"
	}
	return n.str, ""
}

func (r *Resolver) fromKeyring(ctx context.Context, name string) (string, error) {
	if r.Keyring != nil {
		return r.Keyring.Get(KeyringService, name)
	}
	return r.platformKeyring(ctx, name)
}

// provided asks the package serving scheme for a provider reference's value,
// bounded by CommandTimeout, and records the outcome of each call it makes
// (an answer from Cache is not a call). Every failure is a
// safe code: the provider's own refusal code when it is one
// (extension.ValidSecretCode), else provider_refused; provider_failed for
// any other error, whose text is discarded.
func (r *Resolver) provided(ctx context.Context, name, ref, scheme, rest string) (string, error) {
	call := func(ctx context.Context) (string, string) {
		start := time.Now()
		res := SecretResolution{Name: name, Reference: ref, Scheme: scheme, Consumer: r.consumer}
		v, code := r.serve(ctx, &res, scheme, rest)
		if code == "" && v == "" {
			code = "empty"
		}
		if code == "" && len(v) > maxSecretOutput {
			v, code = "", "output_too_large"
		}
		res.Code, res.Duration = code, time.Since(start)
		if r.Observe != nil {
			r.Observe(ctx, res)
		}
		return v, code
	}
	var v, code string
	if r.Cache != nil && !r.fresh {
		v, code = r.Cache.do(ctx, r.consumer+"\x00"+ref, call)
	} else {
		v, code = call(ctx)
	}
	if code != "" {
		return "", &secretError{code: code}
	}
	return v, nil
}

func (r *Resolver) serve(ctx context.Context, res *SecretResolution, scheme, rest string) (string, string) {
	if r.Providers == nil {
		return "", "provider_unavailable"
	}
	p, pkg, ok := r.Providers(scheme)
	if !ok || p == nil {
		return "", "provider_unavailable"
	}
	res.Provider = pkg
	if pkg == r.consumer {
		return "", "provider_cycle"
	}
	timeout := r.CommandTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	v, err := p.Serve(ctx, extension.SecretRequest{Scheme: scheme, Reference: rest})
	var refused *extension.SecretRefusal
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", "timeout"
	case ctx.Err() != nil:
		return "", "cancelled"
	case errors.As(err, &refused) && extension.ValidSecretCode(refused.Code):
		return "", refused.Code
	case errors.As(err, &refused):
		return "", "provider_refused"
	case err != nil:
		return "", "provider_failed"
	}
	return v, ""
}

// ResolveNames resolves only the named secrets of one consumer. A missing or
// removed name fails; there is no fallback to literals, empty values or other
// scopes. The returned map is fresh and owned by the caller.
func (r *Resolver) ResolveNames(ctx context.Context, scope *EffectiveScope, names []string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	for _, name := range names {
		ref := ""
		if scope != nil {
			ref = scope.SecretRefs[name]
		}
		if ref == "" {
			return nil, &secretError{code: "unknown_secret", name: name}
		}
		v, err := r.ResolveNamed(ctx, name, ref)
		if err != nil {
			return nil, &secretError{code: SecretCode(err), name: name}
		}
		out[name] = v
	}
	return out, nil
}

func (r *Resolver) platformKeyring(ctx context.Context, name string) (string, error) {
	env := r.baseEnv()
	for _, k := range []string{"DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR"} {
		if v, ok := r.lookup(k); ok {
			env = append(env, k+"="+v)
		}
	}
	var out, code string
	switch runtime.GOOS {
	case "darwin":
		out, code = r.run(ctx, env, "/usr/bin/security", "find-generic-password", "-s", KeyringService, "-a", name, "-w")
	case "linux":
		out, code = r.run(ctx, env, "secret-tool", "lookup", "service", KeyringService, "account", name)
	default:
		code = "unsupported_platform"
	}
	if code != "" {
		return "", errors.New(code)
	}
	return out, nil
}

// CommandPath is the fixed PATH cmd: references run with: the system
// directories, then the usual Homebrew prefixes (Apple silicon, Intel and
// Linux) so a Homebrew-installed CLI such as gh is found.
const CommandPath = "/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin"

var commandWord = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)

// CommandName returns the program a cmd: reference runs (its first word),
// or "" when ref is not a cmd: reference or does not start with a plain
// program name (an assignment, quoting or shell syntax). Only the program is
// returned, never its arguments.
func CommandName(ref string) string {
	rest, ok := strings.CutPrefix(ref, "cmd:")
	if !ok {
		return ""
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 || !commandWord.MatchString(fields[0]) {
		return ""
	}
	return fields[0]
}

// CommandFound reports whether name is an executable at its absolute path or
// in a CommandPath directory. Shell builtins are not considered.
func CommandFound(name string) bool {
	executable := func(p string) bool {
		fi, err := os.Stat(p)
		return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
	}
	if strings.Contains(name, "/") {
		return filepath.IsAbs(name) && executable(name)
	}
	for _, dir := range filepath.SplitList(CommandPath) {
		if executable(filepath.Join(dir, name)) {
			return true
		}
	}
	return false
}

// Warnings returns what config check warns about from the config's text
// alone, without failing: CommandWarnings. The executors' own warnings
// about their bindings need the validated config (BindingWarnings).
func Warnings(data []byte) []string {
	return CommandWarnings(data)
}

// CommandWarnings returns one warning per program run by a cmd: reference in
// a secret_refs object of the config that CommandFound does not find, sorted
// and naming only the program, never its arguments.
func CommandWarnings(data []byte) []string {
	var root any
	if json.Unmarshal(data, &root) != nil {
		return nil
	}
	missing := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, c := range v {
				if refs, ok := c.(map[string]any); ok && k == "secret_refs" {
					for _, ref := range refs {
						ref, _ := ref.(string)
						if name := CommandName(ref); name != "" && !CommandFound(name) {
							missing[name] = true
						}
					}
					continue
				}
				walk(c)
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(root)
	out := make([]string, 0, len(missing))
	for name := range missing {
		out = append(out, "cmd: reference runs "+name+", which is not found on PATH "+CommandPath+"; give its absolute path")
	}
	sort.Strings(out)
	return out
}

// baseEnv is the minimal command environment: no toolbelt token, endpoint,
// Open Conduit settings or inherited credentials.
func (r *Resolver) baseEnv() []string {
	env := []string{"PATH=" + CommandPath}
	for _, k := range []string{"HOME", "USER", "LOGNAME", "LANG", "TMPDIR"} {
		if v, ok := r.lookup(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// boundedOutput keeps at most limit bytes and cancels the command beyond it.
type boundedOutput struct {
	mu     sync.Mutex
	buf    []byte
	over   bool
	cancel func()
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf)+len(p) > maxSecretOutput {
		b.over = true
		b.cancel()
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// run executes in its own process group, killed on deadline or overflow, with
// stderr discarded. It returns trimmed stdout or a safe failure code.
func (r *Resolver) run(ctx context.Context, env []string, name string, args ...string) (string, string) {
	timeout := r.CommandTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out := &boundedOutput{cancel: cancel}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env, cmd.Stdout, cmd.Dir = env, out, "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	out.mu.Lock()
	defer out.mu.Unlock()
	switch {
	case out.over:
		return "", "output_too_large"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", "timeout"
	case ctx.Err() != nil:
		return "", "cancelled"
	case err != nil:
		// sh exits 127 when the command is not found on PATH.
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 127 {
			return "", "command_not_found"
		}
		return "", "command_failed"
	}
	s := string(out.buf)
	if t, ok := strings.CutSuffix(s, "\r\n"); ok {
		s = t
	} else {
		s = strings.TrimSuffix(s, "\n")
	}
	return s, ""
}
