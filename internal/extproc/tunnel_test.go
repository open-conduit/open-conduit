package extproc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/protocol"
)

// tunnel.url is taken only for a run the host holds, and only as a URL the
// operator's own public_url could be: anything else is refused, so a
// package can't point webhooks at a malformed or credential-bearing URL.
func TestTunnelURLIsCheckedByTheHost(t *testing.T) {
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "acme", Version: "1.0.0", Command: []string{"acme"},
		Provides: protocol.Provides{Tunnels: []protocol.TunnelDecl{{Type: "acme"}}}}
	s, err := NewService(m, Deps{Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	s.tunnels["run-1"] = func(u string) { got = append(got, u) }
	call := func(run, u string) error {
		raw, _ := json.Marshal(protocol.TunnelURLParams{Run: run, URL: u})
		_, err := s.Handle(context.Background(), "tunnel.url", raw)
		return err
	}
	for _, u := range []string{"https://hooks.example.test", ""} {
		if err := call("run-1", u); err != nil {
			t.Fatalf("%q: %v", u, err)
		}
	}
	for _, u := range []string{"https://user:pw@hooks.example.test", "ftp://hooks.example.test", "hooks.example.test", "https://"} {
		if err := call("run-1", u); protocol.ErrorCode(err) != protocol.CodeInvalid {
			t.Errorf("%q: %v", u, err)
		}
	}
	if err := call("run-2", "https://hooks.example.test"); protocol.ErrorCode(err) != protocol.CodeConflict {
		t.Errorf("another run: %v", err)
	}
	if len(got) != 2 || got[0] != "https://hooks.example.test" || got[1] != "" {
		t.Fatalf("published %q", got)
	}
}
