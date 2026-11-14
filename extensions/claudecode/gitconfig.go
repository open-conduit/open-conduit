package claudecode

import (
	"os"
	"sort"
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
