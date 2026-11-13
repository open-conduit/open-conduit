package config

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The stores a SecretStore writes to, as the scheme of the reference that
// reads a value back (keyring:NAME, file:NAME).
const (
	StoreKeyring = "keyring"
	StoreFile    = "file"
)

// MaxSecretValue bounds a stored value, as it bounds what a reference may
// resolve to.
const MaxSecretValue = maxSecretOutput

var (
	// ErrSecretName is a name that is not a valid keyring:/file: name.
	ErrSecretName = errors.New("secret names match [a-z][a-z0-9_-]* (at most 63 characters)")
	// ErrSecretValue is an empty or oversized value.
	ErrSecretValue = errors.New("a secret value is 1 byte to 64 KiB")
	// ErrKeyringRefused is the platform keyring refusing to store or remove.
	ErrKeyringRefused = errors.New("the platform keyring refused the secret")
	// ErrSecretsFile is a secrets file that cannot be read or written.
	ErrSecretsFile = errors.New("the secrets file is unreadable or not a JSON object of strings")
)

// KeyringWriter stores and removes values under a keyring service. Remove
// of a name the keyring does not hold is not an error.
// It returns once ctx is done, so a blocked keyring is a refusal, never a
// hang.
type KeyringWriter interface {
	Set(ctx context.Context, service, name, value string) error
	Remove(ctx context.Context, service, name string) error
}

// keyringTimeout bounds one keyring tool run, as CommandTimeout bounds a
// resolver's.
const keyringTimeout = 10 * time.Second

// SecretStore writes the values keyring: and file: references read: the
// platform keyring under KeyringService, and the owner-only secrets file a
// Resolver's SecretsFile names. It never logs, echoes or returns a value.
type SecretStore struct {
	File    string        // owner-only secrets.json
	Keyring KeyringWriter // nil uses the platform keyring
	mu      sync.Mutex
}

// ValidSecretName reports whether name may be stored (and referenced as
// keyring:NAME or file:NAME).
func ValidSecretName(name string) bool { return idPattern.MatchString(name) }

// Set stores value under name and returns the reference that reads it.
// store is StoreKeyring, StoreFile or "" for the keyring, falling back to
// the file when no keyring is usable (fellBack reports that it did).
func (s *SecretStore) Set(ctx context.Context, name, value, store string) (ref string, fellBack bool, err error) {
	if !ValidSecretName(name) {
		return "", false, ErrSecretName
	}
	if value == "" || len(value) > MaxSecretValue {
		return "", false, ErrSecretValue
	}
	switch store {
	case "", StoreKeyring:
		if err := s.keyring().Set(ctx, KeyringService, name, value); err == nil {
			return StoreKeyring + ":" + name, false, nil
		} else if store == StoreKeyring {
			return "", false, ErrKeyringRefused
		}
		fellBack = true
	case StoreFile:
	default:
		return "", false, fmt.Errorf("store is %s or %s", StoreKeyring, StoreFile)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	values, err := s.readFile()
	if err != nil {
		return "", false, err
	}
	values[name] = value
	if err := s.writeFile(values); err != nil {
		return "", false, err
	}
	return StoreFile + ":" + name, fellBack, nil
}

// Remove removes name from store. Removing a name the store does not hold
// succeeds.
func (s *SecretStore) Remove(ctx context.Context, name, store string) error {
	if !ValidSecretName(name) {
		return ErrSecretName
	}
	switch store {
	case StoreKeyring:
		if s.keyring().Remove(ctx, KeyringService, name) != nil {
			return ErrKeyringRefused
		}
		return nil
	case StoreFile:
		s.mu.Lock()
		defer s.mu.Unlock()
		values, err := s.readFile()
		if err != nil {
			return err
		}
		if _, ok := values[name]; !ok {
			return nil
		}
		delete(values, name)
		return s.writeFile(values)
	}
	return fmt.Errorf("store is %s or %s", StoreKeyring, StoreFile)
}

// FileNames are the names the secrets file holds, sorted. The keyring
// cannot be listed without reading its values, so it has no counterpart.
func (s *SecretStore) FileNames() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values, err := s.readFile()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

func (s *SecretStore) keyring() KeyringWriter {
	if s.Keyring != nil {
		return s.Keyring
	}
	return PlatformKeyring
}

func (s *SecretStore) readFile() (map[string]string, error) {
	values := map[string]string{}
	if s.File == "" {
		return nil, ErrSecretsFile
	}
	data, err := ReadFile(s.File)
	if errors.Is(err, fs.ErrNotExist) {
		return values, nil
	}
	if err != nil || json.Unmarshal(data, &values) != nil {
		return nil, ErrSecretsFile
	}
	return values, nil
}

// writeFile replaces the secrets file atomically, owner-only, one sorted
// name per line.
func (s *SecretStore) writeFile(values map[string]string) error {
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, k := range names {
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(values[k])
		fmt.Fprintf(&b, "  %s: %s", kb, vb)
		if i < len(names)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
	if os.MkdirAll(filepath.Dir(s.File), 0o700) != nil || replace(s.File, b.Bytes()) != nil {
		return ErrSecretsFile
	}
	return nil
}

// PlatformKeyring is the platform's keyring: macOS's security tool or
// libsecret's secret-tool on Linux.
var PlatformKeyring KeyringWriter = platformKeyringWriter{}

// platformKeyringWriter drives the platform's keyring tool. The value
// travels on stdin, never in arguments; each run has a deadline and the
// resolver's minimal environment, in its own process group, killed whole.
type platformKeyringWriter struct{}

func keyringCommand(ctx context.Context, stdin string, name string, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, keyringTimeout)
	cmd := exec.CommandContext(ctx, name, args...)
	env := []string{"PATH=" + CommandPath}
	for _, k := range []string{"HOME", "USER", "LOGNAME", "LANG", "TMPDIR", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env, cmd.Dir = env, "/"
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	return cmd, cancel
}

func (platformKeyringWriter) Set(ctx context.Context, service, name, value string) error {
	var cmd *exec.Cmd
	var cancel context.CancelFunc
	switch runtime.GOOS {
	case "darwin":
		cmd, cancel = keyringCommand(ctx, fmt.Sprintf("add-generic-password -U -s %s -a %s -X %s\n", service, name, hex.EncodeToString([]byte(value))), "/usr/bin/security", "-i")
	case "linux":
		cmd, cancel = keyringCommand(ctx, value, "secret-tool", "store", "--label="+service+" "+name, "service", service, "account", name)
	default:
		return errors.New("no platform keyring")
	}
	defer cancel()
	return cmd.Run()
}

func (platformKeyringWriter) Remove(ctx context.Context, service, name string) error {
	switch runtime.GOOS {
	case "darwin":
		cmd, cancel := keyringCommand(ctx, "", "/usr/bin/security", "delete-generic-password", "-s", service, "-a", name)
		defer cancel()
		err := cmd.Run()
		// security exits 44 when no such item exists.
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 44 {
			return nil
		}
		return err
	case "linux":
		// secret-tool clear succeeds whether or not an item matched.
		cmd, cancel := keyringCommand(ctx, "", "secret-tool", "clear", "service", service, "account", name)
		defer cancel()
		return cmd.Run()
	}
	return errors.New("no platform keyring")
}
