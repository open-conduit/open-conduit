package sshext

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/open-conduit/open-conduit/extensions/ssh"
	"github.com/open-conduit/open-conduit/protocol"
)

// TestConfiguredRemotes: the provider reaches the remotes the last
// configure's ssh bindings name, and nothing of another provider's or an
// invalid config.
func TestConfiguredRemotes(t *testing.T) {
	cases := Certification().Fixture.Containers
	x := &provider{}
	x.configure([]protocol.ContainerUse{
		{Provider: ssh.Type, Config: cases[0].Config},
		{Provider: "docker", Config: cases[0].Config},
		{Provider: ssh.Type, Config: cases[1].Config},
	})
	got := x.configured()
	if len(got) != 1 || got[0].Host != "runner.test" || got[0].Port != 22 {
		t.Fatalf("remotes %+v", got)
	}
}

// TestCertificationCases: the fixture's verdicts are the provider's own.
func TestCertificationCases(t *testing.T) {
	for i, c := range Certification().Fixture.Containers {
		if err := ssh.ValidateConfig(json.RawMessage(c.Config)); (err != nil) != c.Refused {
			t.Fatalf("case %d: %v, refused %t", i, err, c.Refused)
		}
	}
}

func TestCheckNotConfigured(t *testing.T) {
	got := (&provider{}).check(context.Background(), protocol.CheckConfig{})
	if !got.OK || got.Code != "not_configured" {
		t.Fatalf("check %+v", got)
	}
}
