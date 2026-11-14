package cli

import (
	"bytes"
	"strings"
	"testing"
)

// os git-credential, git's credential helper before the git transport,
// answers nothing for any request and asks the toolbelt nothing: git signs
// in through the host, so a run config that still names the helper only
// makes git ask its next one.
func TestToolbeltGitCredentialAnswersNothing(t *testing.T) {
	asked := false
	env := Env{Getenv: func(k string) string {
		asked = asked || strings.HasPrefix(k, "OS_")
		return ""
	}}
	for _, op := range []string{"get", "store", "erase"} {
		var out, errOut bytes.Buffer
		code := RunToolbelt("t", "c", []string{"git-credential", op}, env, strings.NewReader("protocol=https\nhost=github.com\n\n"), &out, &errOut)
		if code != 0 || out.Len() != 0 || errOut.Len() != 0 {
			t.Fatalf("%s: %d %q %q", op, code, out.String(), errOut.String())
		}
	}
	if asked {
		t.Fatal("git-credential reached for the toolbelt")
	}
	var out, errOut bytes.Buffer
	if code := RunToolbelt("t", "c", []string{"git-credential"}, env, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("no action: %d", code)
	}
}
