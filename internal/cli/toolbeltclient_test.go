package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// A toolbelt whose token file has gone (the host closed the execution's run
// directory) says the execution has ended, not that the toolbelt was never
// set up; one with neither variable says it runs inside an execution.
func TestToolbeltSaysWhyItHasNoToken(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "token", "token")
	for name, c := range map[string]struct {
		vars map[string]string
		want string
	}{
		"token file gone": {map[string]string{"OS_ENDPOINT": "unix:///nowhere.sock", "OS_TOKEN_FILE": gone}, "this execution has ended"},
		"never set":       {map[string]string{}, "OS_ENDPOINT and OS_TOKEN are not set"},
	} {
		t.Run(name, func(t *testing.T) {
			env := Env{Getenv: func(k string) string { return c.vars[k] }}
			var out, errOut bytes.Buffer
			if code := RunToolbelt("t", "c", []string{"context"}, env, nil, &out, &errOut); code != 2 {
				t.Fatalf("exit %d: %s", code, errOut.String())
			}
			if !strings.Contains(errOut.String(), c.want) {
				t.Fatalf("error %q does not say %q", errOut.String(), c.want)
			}
		})
	}
}
