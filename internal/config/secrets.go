package config

import (
	"context"
	"encoding/json"
	"errors"
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

// Resolver resolves references lazily for one consumer. It keeps no cache.
type Resolver struct {
	Keyring        Keyring // nil uses the platform keyring helper
	SecretsFile    string  // owner-only secrets.json for file: references
	LookupEnv      func(string) (string, bool)
	CommandTimeout time.Duration // default 10s
}

func (r *Resolver) lookup(k string) (string, bool) {
	if r.LookupEnv != nil {
		return r.LookupEnv(k)
	}
	return os.LookupEnv(k)
}
func (r *Resolver) Resolve(ctx context.Context, ref string) (string, error) {
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
	case "file":
		data, err := ReadFile(r.SecretsFile)
		if err != nil || r.SecretsFile == "" {
			return "", &secretError{code: "file_unavailable"}
		}
		root, diags := decode(data)
		if root == nil || len(diags) > 0 || root.kind != kObject {
			return "", &secretError{code: "file_invalid"}
		}
		n := root.field(rest)
		if n == nil {
			return "", &secretError{code: "file_missing"}
		}
		if n.kind != kString {
			return "", &secretError{code: "file_invalid"}
		}
		v = n.str
	case "keyring":
		var err error
		if r.Keyring != nil {
			v, err = r.Keyring.Get(KeyringService, rest)
		} else {
			v, err = r.platformKeyring(ctx, rest)
		}
		if err != nil {
			return "", &secretError{code: "keyring_unavailable"}
		}
	case "cmd":
		out, code := r.run(ctx, r.baseEnv(), "sh", "-c", rest)
		if code != "" {
			return "", &secretError{code: code}
		}
		v = out
	}
	if v == "" {
		return "", &secretError{code: "empty"}
	}
	return v, nil
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
		v, err := r.Resolve(ctx, ref)
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

// Warnings returns everything config check warns about without failing:
// CommandWarnings, then UnenforcedWarnings.
func Warnings(data []byte) []string {
	return append(CommandWarnings(data), UnenforcedWarnings(data)...)
}

// UnenforcedWarnings returns one warning per admission_limit the config sets,
// in settings or a scope's limits: it is validated and shown in the effective
// config, but nothing yet caps the Work a scope admits (#39).
func UnenforcedWarnings(data []byte) []string {
	var doc struct {
		Settings map[string]json.RawMessage `json:"settings"`
		Scopes   []struct {
			ID     string                     `json:"id"`
			Limits map[string]json.RawMessage `json:"limits"`
		} `json:"scopes"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	const note = " has no effect yet: Open Conduit does not cap the Work a scope admits; remove it, or use concurrency to cap running attempts"
	var out []string
	if _, ok := doc.Settings["admission_limit"]; ok {
		out = append(out, "settings.admission_limit"+note)
	}
	for _, sc := range doc.Scopes {
		if _, ok := sc.Limits["admission_limit"]; ok {
			out = append(out, "scope "+sc.ID+" limits.admission_limit"+note)
		}
	}
	return out
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
