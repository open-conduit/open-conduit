package extension

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// RunEnvironmentRequest asks an extension for its environment layer for one
// run: the Work being run and the execution, in a scope that uses the
// extension, and the connection the run's binding names ("" for none), as
// an operation's Authority.Connection carries it. The extension owning that
// connection's type can resolve its config and secrets in the scope.
type RunEnvironmentRequest struct {
	Owner      ResourceOwner
	Connection string
	// Binding is the name of the executor binding the run is on, as the
	// host stored it.
	Binding string
	// FilesDir is the directory the host writes the layer's files to
	// (RunLayer.Files), so an environment variable can name one: a
	// host-kept directory of the run's own that the run reads at the
	// same path, on the host, in a process sandbox and in a container.
	// It is empty when the run has none (a binding with no toolbelt),
	// and then files are not written.
	FilesDir string
}

// RunLayer is an extension's contribution to a run: environment
// variables, as RunEnvironment returns them, and files the host writes,
// before the run starts, into the request's FilesDir, each under its name
// (CheckRunFiles). A file is the run's to read, never to change: the host
// writes it owner-only and removes it with the run's directory.
type RunLayer struct {
	Env   map[string]string
	Files map[string]string
}

// RunLayerProvider is an extension whose run layer can carry files as well
// as environment variables (protocol run.environment's files). The host
// asks it instead of RunEnvironment.
type RunLayerProvider interface {
	RunLayer(ctx context.Context, r RunEnvironmentRequest) (RunLayer, error)
}

// Run file limits: a few small config files, not a channel for data.
const (
	MaxRunFiles     = 16
	MaxRunFileBytes = 64 << 10
)

var runFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// CheckRunFiles reports whether files may be written into a run: at most
// MaxRunFiles, each a plain file name (letters, digits, '.', '_' and '-',
// never a path) of at most MaxRunFileBytes with no NUL.
func CheckRunFiles(files map[string]string) error {
	if len(files) > MaxRunFiles {
		return fmt.Errorf("a run layer may carry at most %d files", MaxRunFiles)
	}
	for name, content := range files {
		if !runFileName.MatchString(name) || name == "." || name == ".." {
			return fmt.Errorf("run file %q is not a plain file name", name)
		}
		if len(content) > MaxRunFileBytes || strings.ContainsRune(content, 0) {
			return fmt.Errorf("run file %q is over %d bytes or has a NUL", name, MaxRunFileBytes)
		}
	}
	return nil
}

// RunEnvironment contributes an environment layer to the runs of Work in
// the scopes that use the extension (protocol run.environment). The host
// merges every extension's layer into ExecutionContext.Env before the
// executor starts; an error fails the run before anything starts. A layer
// that needs the toolbelt reaches it through `os` (git's transport,
// GitTransportLayer, say): executors give a process that applies the layer
// the toolbelt client, OS_ENDPOINT and OS_TOKEN_FILE.
type RunEnvironment interface {
	RunEnvironment(ctx context.Context, r RunEnvironmentRequest) (map[string]string, error)
}

// GitTransport is the name git knows the toolbelt's git transport by: a
// remote URL "open-conduit::https://host/owner/repo" runs `os git-remote`
// (through the git alias remote-open-conduit), which carries git's smart
// HTTP to the repository through the host. The host signs each request in
// with what an operation serving CapabilityGitCredential answers for the
// repository, so git reaches it without the credential ever entering the
// run.
const GitTransport = "open-conduit"

// GitTransportLayer is the git config, as an environment layer, that sends
// git's https remotes on each of hosts through the toolbelt's git
// transport: a URL rewrite per host, the alias that makes `os git-remote`
// the transport's remote helper, and leave for submodules on those hosts
// to use it. An extension that serves CapabilityGitCredential for a host
// returns it from RunEnvironment. A remote the extension gives no
// credential for is fetched directly, as git would without the layer.
func GitTransportLayer(hosts ...string) map[string]string {
	pairs := [][2]string{
		{"alias.remote-" + GitTransport, "!os git-remote"},
		{"protocol." + GitTransport + ".allow", "always"},
	}
	for _, h := range hosts {
		pairs = append(pairs, [2]string{"url." + GitTransport + "::https://" + h + "/.insteadOf", "https://" + h + "/"})
	}
	out := map[string]string{"GIT_CONFIG_COUNT": strconv.Itoa(len(pairs))}
	for i, p := range pairs {
		out[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)], out[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)] = p[0], p[1]
	}
	return out
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ApplyEnv applies an environment layer (a resource provider's, or the
// extensions' run layer) over vars. A name that isn't a variable name, a
// reserved toolbelt variable (OS_*) and a value with a NUL are skipped.
// git's GIT_CONFIG_COUNT entries are added after the ones vars already has,
// so a run layer's git config extends a workspace's instead of replacing
// it; every other variable replaces the earlier value.
func ApplyEnv(vars, layer map[string]string) {
	have, _ := strconv.Atoi(vars["GIT_CONFIG_COUNT"])
	add, _ := strconv.Atoi(layer["GIT_CONFIG_COUNT"])
	extend := have > 0 && add > 0
	for name, value := range layer {
		if extend && (name == "GIT_CONFIG_COUNT" || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")) {
			continue
		}
		if envName.MatchString(name) && !strings.HasPrefix(strings.ToUpper(name), "OS_") && !strings.ContainsRune(value, 0) {
			vars[name] = value
		}
	}
	if !extend {
		return
	}
	n := have
	for i := 0; i < add; i++ {
		k, ok := layer[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)]
		v := layer[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)]
		if !ok || strings.ContainsRune(k+v, 0) {
			continue
		}
		vars[fmt.Sprintf("GIT_CONFIG_KEY_%d", n)], vars[fmt.Sprintf("GIT_CONFIG_VALUE_%d", n)] = k, v
		n++
	}
	vars["GIT_CONFIG_COUNT"] = strconv.Itoa(n)
}
