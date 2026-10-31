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
// extension.
type RunEnvironmentRequest struct {
	Owner ResourceOwner
}

// RunEnvironment contributes an environment layer to the runs of Work in
// the scopes that use the extension (protocol run.environment). The host
// merges every extension's layer into ExecutionContext.Env before the
// executor starts; an error fails the run before anything starts. A layer
// that needs the toolbelt reaches it through `os` (git's `!os
// git-credential`, say): executors give a process that applies the layer
// the toolbelt client, OS_ENDPOINT and OS_TOKEN_FILE.
type RunEnvironment interface {
	RunEnvironment(ctx context.Context, r RunEnvironmentRequest) (map[string]string, error)
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
