package cli

import (
	"bytes"
	"strings"
	"testing"
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
	for _, want := range []string{"retired", "-c 'alias.remote-open-conduit=!os git-remote'", "-c protocol.open-conduit.allow=always", "-c url.open-conduit::https://github.com/.insteadOf=https://github.com/"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("hint %q lacks %q", hint, want)
		}
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
