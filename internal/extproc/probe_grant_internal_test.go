package extproc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/protocol"
)

// TestProbeGrantEndsWithTheRun checks that a probe's grant reads only its
// own connection in its own scope, and nothing once the run is over.
func TestProbeGrantEndsWithTheRun(t *testing.T) {
	m := protocol.Manifest{Protocol: []int{protocol.Version}, Name: "acme", Version: "1.0.0", Command: []string{"./acme"}}
	s, err := NewService(m, Deps{Now: time.Now, Scopes: func() []domain.ID { return nil },
		ProbeConnection: func(scope domain.ID, id string) (protocol.ConnectionEntry, bool) {
			return protocol.ConnectionEntry{ID: id, Type: "acme"}, scope == "eng" && id == "conn"
		},
		ProbeSecrets: func(context.Context, domain.ID, string, []string) (map[string]string, error) {
			return map[string]string{"token": "value"}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), probeScopeKey{}, domain.ID("eng"))
	if _, err = s.grantProbeConnection(ctx, &protocol.ProbeRunParams{}, "other"); err == nil {
		t.Fatal("granted a connection the scope doesn't have")
	}
	if _, err = s.grantProbeConnection(context.Background(), &protocol.ProbeRunParams{}, "conn"); err == nil {
		t.Fatal("granted a connection with no scope checked by probe.run")
	}
	var params protocol.ProbeRunParams
	closeGrant, err := s.grantProbeConnection(ctx, &params, "conn")
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(scope, connection string) error {
		raw, _ := json.Marshal(protocol.SecretResolveParams{ScopeID: scope, Connection: connection, ProbeGrant: params.Grant, Names: []string{"token"}})
		_, err := s.Handle(ctx, "secret.resolve", raw)
		return err
	}
	if err = resolve("eng", "conn"); err != nil {
		t.Fatalf("during the run: %v", err)
	}
	if resolve("other", "conn") == nil || resolve("eng", "other") == nil {
		t.Fatal("the grant read outside its scope and connection")
	}
	closeGrant()
	if resolve("eng", "conn") == nil {
		t.Fatal("the grant outlived the run")
	}
}
