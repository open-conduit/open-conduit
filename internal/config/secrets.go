package config

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
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

// baseEnv is the minimal command environment: no toolbelt token, endpoint,
// Open Conduit settings or inherited credentials.
func (r *Resolver) baseEnv() []string {
	env := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
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
