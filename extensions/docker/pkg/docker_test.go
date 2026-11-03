package dockerext

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extensions/docker"
	"github.com/open-conduit/open-conduit/protocol"
)

func config(dctx string) json.RawMessage {
	return json.RawMessage(`{"image":"alpine@sha256:` + strings.Repeat("0", 64) + `","provider":{"context":"` + dctx + `"}}`)
}

// TestConfiguredContexts: the provider lists on the contexts the last
// configure's docker bindings name, once each, and nothing of another
// provider's or an invalid config.
func TestConfiguredContexts(t *testing.T) {
	x := &provider{}
	x.configure([]protocol.ContainerUse{
		{Provider: docker.Type, Config: config("a")},
		{Provider: docker.Type, Config: config("b")},
		{Provider: docker.Type, Config: config("a")},
		{Provider: "ssh", Config: config("c")},
		{Provider: docker.Type, Config: json.RawMessage(`{"image":"x"}`)},
	})
	if got := x.configured(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("contexts %v", got)
	}
	x.configure(nil)
	if got := x.configured(); len(got) != 0 {
		t.Fatalf("contexts after a configure with none: %v", got)
	}
}

// TestForward: a unix forward needs no address (the gateway mounts the
// socket); a tcp one is refused for a config that names no context.
func TestForward(t *testing.T) {
	cfg := domain.Payload{Data: config("default")}
	if addr, err := docker.New(docker.Options{Forward: docker.ForwardUnix}).Forward(context.Background(), cfg); err != nil || addr != "" {
		t.Fatalf("unix forward: %q %v", addr, err)
	}
	if _, err := docker.New(docker.Options{Forward: docker.ForwardTCP}).Forward(context.Background(), domain.Payload{Data: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("a tcp forward for an invalid config was answered")
	}
}

// TestCheckNotConfigured: with no docker binding the check says so and
// asks nothing of Docker.
func TestCheckNotConfigured(t *testing.T) {
	x := &provider{}
	got := x.check(context.Background(), protocol.CheckConfig{Scopes: []protocol.CheckScope{{ID: "s", Bindings: []protocol.CheckBinding{{Name: "b", Executor: "shell"}}}}})
	if !got.OK || got.Code != "not_configured" {
		t.Fatalf("check %+v", got)
	}
}
