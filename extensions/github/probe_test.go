package github

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
)

func TestProbes(t *testing.T) {
	var ran []string
	p := Probes{Gh: func(_ context.Context, args ...string) ([]byte, error) {
		ran = append(ran, strings.Join(args, " "))
		switch args[0] {
		case "auth":
			return []byte(`{"hosts":{"github.com":[{"host":"github.com","login":"octo","active":true,"tokenSource":"keyring","state":"success","token":"gho_secret","scopes":"repo"}]}}`), nil
		case "repo":
			return []byte(`[{"nameWithOwner":"octo/widgets"},{"nameWithOwner":"octo/gadgets"}]`), nil
		}
		return nil, errors.New("unexpected")
	}}
	probes := map[string]extension.SetupProbe{}
	for _, x := range p.SetupProbes() {
		probes[x.Name] = x
	}
	r, err := probes["accounts"].Run(context.Background(), nil)
	b, _ := json.Marshal(r.Output)
	if err != nil || !strings.Contains(string(b), `"login":"octo","active":true,"token_source":"keyring","state":"success"`) || strings.Contains(string(b), "gho_") {
		t.Fatalf("accounts: %s %v", b, err)
	}
	r, err = probes["repos"].Run(context.Background(), map[string]string{"owner": "octo"})
	b, _ = json.Marshal(r.Output)
	if err != nil || string(b) != `{"repos":["octo/widgets","octo/gadgets"]}` || r.Command != "gh repo list octo" {
		t.Fatalf("repos: %s %v", b, err)
	}
	for _, owner := range []string{"", "--help", "octo; rm -rf", "a/b"} {
		if _, err = probes["repos"].Run(context.Background(), map[string]string{"owner": owner}); !errors.Is(err, extension.ErrProbeInput) {
			t.Errorf("owner %q: %v", owner, err)
		}
	}
	if strings.Join(ran, "|") != "auth status --json hosts|repo list octo --limit 200 --json nameWithOwner" {
		t.Fatalf("ran: %q", ran)
	}
}
