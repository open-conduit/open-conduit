package cli

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

// os git-credential, git's credential helper before the git transport,
// answers nothing for any request and asks the toolbelt nothing: git signs
// in through the host, so a run config that still names the helper only
// makes git ask its next one. Such a config is one a daemon from before
// the transport set for a run a newer daemon adopted, so `get` for an https
// host tells git's user, on stderr, the git options that send the command
// through the transport; store, erase and other protocols say nothing.
func TestToolbeltGitCredentialAnswersNothing(t *testing.T) {
	asked := false
	env := Env{Getenv: func(k string) string {
		asked = asked || strings.HasPrefix(k, "OS_")
		return ""
	}}
	run := func(op, in string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := RunToolbelt("t", "c", []string{"git-credential", op}, env, strings.NewReader(in), &out, &errOut)
		return code, out.String(), errOut.String()
	}
	https := "protocol=https\nhost=github.com\n\n"
	for _, op := range []string{"get", "store", "erase"} {
		code, out, errOut := run(op, https)
		if code != 0 || out != "" {
			t.Fatalf("%s: %d %q", op, code, out)
		}
		if hinted := errOut != ""; hinted != (op == "get") {
			t.Fatalf("%s: stderr %q", op, errOut)
		}
	}
	_, _, hint := run("get", "protocol=https\r\nhost=github.com\r\n\r\n")
	// The options are the transport's own layer, so a change to it reaches
	// adopted runs too.
	layer := extension.GitTransportLayer("github.com")
	n, _ := strconv.Atoi(layer["GIT_CONFIG_COUNT"])
	if n == 0 || !strings.Contains(hint, "retired") {
		t.Fatalf("hint %q", hint)
	}
	for i := 0; i < n; i++ {
		want := "-c '" + layer[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)] + "=" + layer[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)] + "'"
		if !strings.Contains(hint, want) {
			t.Fatalf("hint %q lacks %q", hint, want)
		}
	}
	if strings.Count(hint, "-c '") != n {
		t.Fatalf("hint %q has options beyond the layer's %d", hint, n)
	}
	for _, in := range []string{"protocol=http\nhost=github.com\n\n", "protocol=ssh\nhost=github.com\n\n", "protocol=https\n\n", "protocol=https\nhost=x'; rm -rf ~\n\n", ""} {
		if code, out, errOut := run("get", in); code != 0 || out != "" || errOut != "" {
			t.Fatalf("get %q: %d %q %q", in, code, out, errOut)
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
