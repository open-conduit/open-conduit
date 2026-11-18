package extproc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

type recordingSite struct {
	id  string
	ran []extension.SiteCommand
}

func (s *recordingSite) ID() string { return s.id }
func (s *recordingSite) Run(_ context.Context, cmd extension.SiteCommand) (extension.SiteResult, error) {
	s.ran = append(s.ran, cmd)
	return extension.SiteResult{ExitCode: 0, Stdout: []byte("ok")}, nil
}

// site.run reaches only a site the host gave: an acquisition's, under its
// call while it runs, or one the host resolves for a resource type the
// package declares with sites. Anything else is refused.
func TestSiteRunIsBoundByTheHost(t *testing.T) {
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "acme", Version: "1.0.0", Command: []string{"acme"},
		Provides: protocol.Provides{Resources: []protocol.ResourceDecl{{Type: "acme", Modes: []string{"work"}, Sites: true}, {Type: "acme-plain", Modes: []string{"work"}}}}}
	resolved := &recordingSite{id: "pod/b"}
	var asked []string
	s, err := NewService(m, Deps{Now: time.Now, Sites: func(_ context.Context, typ, id string) (extension.Site, error) {
		asked = append(asked, typ+" "+id)
		if id != resolved.id {
			return nil, extension.ErrSiteUnavailable
		}
		return resolved, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	acquired := &recordingSite{id: "pod/a"}
	s.sites["call-1"] = acquireSite{typ: "acme", site: acquired}
	call := func(p protocol.SiteRunParams) (protocol.SiteResult, error) {
		p.Command = protocol.SiteCommand{Argv: []string{"true"}}
		raw, _ := json.Marshal(p)
		out, err := s.Handle(context.Background(), "site.run", raw)
		r, _ := out.(protocol.SiteResult)
		return r, err
	}
	if r, err := call(protocol.SiteRunParams{Type: "acme", Call: "call-1"}); err != nil || string(r.Stdout) != "ok" || len(acquired.ran) != 1 {
		t.Fatalf("the acquisition's site: %+v %v", r, err)
	}
	if _, err := call(protocol.SiteRunParams{Type: "acme", Call: "call-1", Site: "pod/b"}); protocol.ErrorCode(err) != protocol.CodeUnauthorized {
		t.Errorf("a call naming another site: %v", err)
	}
	if _, err := call(protocol.SiteRunParams{Type: "acme", Call: "call-2"}); protocol.ErrorCode(err) != protocol.CodeConflict {
		t.Errorf("a call no acquisition holds: %v", err)
	}
	if _, err := call(protocol.SiteRunParams{Type: "acme-plain", Call: "call-1"}); protocol.ErrorCode(err) != protocol.CodeUnauthorized {
		t.Errorf("a type declared without sites: %v", err)
	}
	if _, err := call(protocol.SiteRunParams{Type: "acme", Site: "pod/b"}); err != nil || len(resolved.ran) != 1 {
		t.Errorf("a site the host resolves: %v", err)
	}
	if _, err := call(protocol.SiteRunParams{Type: "acme", Site: "pod/c"}); !errors.Is(err, extension.ErrSiteUnavailable) {
		t.Errorf("a site the host refuses: %v", err)
	}
	if _, err := call(protocol.SiteRunParams{Type: "acme"}); protocol.ErrorCode(err) != protocol.CodeInvalid {
		t.Errorf("no call and no site: %v", err)
	}
	if len(asked) != 2 || asked[0] != "acme pod/b" {
		t.Fatalf("the host was asked %v", asked)
	}
}
