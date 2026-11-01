package claudecode

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// withToolbelt gives a host process the toolbelt client: `os` on PATH, the
// run's endpoint and its token file, as a container's and the shell
// executor's processes have.
func withToolbelt(env []string, toolbeltDir, endpoint, tokenFile string) []string {
	path, _ := lookupEnv(env, "PATH")
	env = setEnv(env, "PATH="+toolbeltDir+string(os.PathListSeparator)+path)
	env = setEnv(env, "OS_ENDPOINT="+endpoint)
	env = setEnv(env, "OS_TOKEN_FILE="+tokenFile)
	sort.Strings(env)
	return env
}

// gitConfig adds git config entries through GIT_CONFIG_COUNT after those
// env already sets (the workspace's signing policy, a binding's own), so
// both apply.
func gitConfig(env []string, pairs ...[2]string) []string {
	n := 0
	if v, ok := lookupEnv(env, "GIT_CONFIG_COUNT"); ok {
		if c, err := strconv.Atoi(v); err == nil && c > 0 {
			n = c
		}
	}
	for i, p := range pairs {
		env = setEnv(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n+i, p[0]))
		env = setEnv(env, fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n+i, p[1]))
	}
	return setEnv(env, "GIT_CONFIG_COUNT="+strconv.Itoa(n+len(pairs)))
}

// generalCredentialHelper reports whether env's GIT_CONFIG entries already
// set git's general credential.helper (one for a single URL,
// credential.<url>.helper, doesn't count).
func generalCredentialHelper(env []string) bool {
	v, _ := lookupEnv(env, "GIT_CONFIG_COUNT")
	n, _ := strconv.Atoi(v)
	for i := 0; i < n; i++ {
		k, _ := lookupEnv(env, fmt.Sprintf("GIT_CONFIG_KEY_%d", i))
		if strings.EqualFold(k, "credential.helper") {
			return true
		}
	}
	return false
}

func lookupEnv(env []string, name string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

// setEnv sets kv's variable in env, replacing any earlier value.
func setEnv(env []string, kv string) []string {
	name, _, _ := strings.Cut(kv, "=")
	out := env[:0:0]
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); k != name {
			out = append(out, e)
		}
	}
	return append(out, kv)
}
