package sdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

func manifest() protocol.Manifest {
	return protocol.Manifest{Protocol: []int{1}, Name: "chat", Version: "1", Command: []string{"./chat"}, Provides: protocol.Provides{
		Families: []protocol.Family{{Type: "chat.message.posted", ReDerivable: true}},
		Schemas:  []protocol.SchemaDecl{{Name: "chat.message", Versions: []int{1, 2, 3}}},
	}}
}

func serve(t *testing.T, ext *sdk.Extension) (*jsonrpc.Conn, *sdk.Server, *io.PipeWriter) {
	t.Helper()
	sr, hw := io.Pipe()
	hr, sw := io.Pipe()
	s := sdk.NewServer(ext, sr, sw)
	host := jsonrpc.NewConn(hr, hw, func(context.Context, string, json.RawMessage) (any, error) { return struct{}{}, nil }, jsonrpc.Options{})
	t.Cleanup(func() { _ = host.Close() })
	return host, s, hw
}

func TestHandshakeVersionsAndLifecycle(t *testing.T) {
	host, s, stdin := serve(t, &sdk.Extension{Manifest: manifest()})
	ctx := context.Background()
	if err := host.Call(ctx, "configure", protocol.ConfigureParams{}, nil); err == nil {
		t.Fatal("configure before initialize accepted")
	}
	if err := host.Call(ctx, "initialize", protocol.InitializeParams{ProtocolVersions: []int{7}}, nil); !errors.Is(err, protocol.ErrIncompatible) {
		t.Fatalf("no common version: %v", err)
	}
	var init protocol.InitializeResult
	if err := host.Call(ctx, "initialize", protocol.InitializeParams{ProtocolVersions: []int{1, 7}, StateDir: "/state"}, &init); err != nil || init.ProtocolVersion != 1 {
		t.Fatalf("initialize: %+v %v", init, err)
	}
	if m, err := protocol.ParseManifest(init.Manifest); err != nil || m.Name != "chat" {
		t.Fatalf("manifest: %v", err)
	}
	if s.StateDir() != "/state" {
		t.Fatal("state dir")
	}
	if err := host.Call(ctx, "shutdown", struct{}{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.Call(ctx, "configure", protocol.ConfigureParams{}, nil); protocol.ErrorCode(err) != protocol.CodeShuttingDown {
		t.Fatalf("after shutdown: %v", err)
	}
	// EOF on stdin ends the server: an extension never outlives its host.
	_ = stdin.Close()
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("server outlived stdin")
	}
}

func TestUpcastChains(t *testing.T) {
	ups := map[string]sdk.Upcaster{"chat.message": func(from int, data json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(string(data[:len(data)-1]) + `,"v` + string(rune('0'+from+1)) + `":true}`), nil
	}}
	got, err := sdk.Upcast(manifest(), ups, domain.Payload{Schema: "chat.message.v1", Data: []byte(`{"a":1}`)})
	if err != nil || got.Schema != "chat.message.v3" || string(got.Data) != `{"a":1,"v2":true,"v3":true}` {
		t.Fatalf("upcast %s %s %v", got.Schema, got.Data, err)
	}
	for _, s := range []string{"chat.message.v4", "chat.other.v1"} {
		if _, err := sdk.Upcast(manifest(), ups, domain.Payload{Schema: s}); protocol.ErrorCode(err) != protocol.CodeSchema {
			t.Errorf("%s: %v", s, err)
		}
	}
	if p, err := sdk.Upcast(manifest(), nil, domain.Payload{Schema: "github.issue.v1"}); err != nil || p.Schema != "github.issue.v1" {
		t.Error("foreign payload changed")
	}
	if _, err := sdk.Upcast(manifest(), nil, domain.Payload{Schema: "chat.message.v2"}); protocol.ErrorCode(err) != protocol.CodeSchema {
		t.Error("missing upcaster not refused")
	}
}

func TestKVMigrationRunsBeforeConfigure(t *testing.T) {
	var order []string
	m := manifest()
	m.KVVersion = 2
	ext := &sdk.Extension{Manifest: m,
		Migrate: func(_ context.Context, _ *sdk.Host, from, to int) error {
			order = append(order, "migrate")
			if from != 1 || to != 2 {
				t.Errorf("migrate %d→%d", from, to)
			}
			return nil
		},
		Configure: func(context.Context, *sdk.Host, protocol.ConfigureParams) (protocol.ConfigureResult, error) {
			order = append(order, "configure")
			return protocol.ConfigureResult{}, nil
		}}
	host, _, _ := serve(t, ext)
	ctx := context.Background()
	if err := host.Call(ctx, "initialize", protocol.InitializeParams{ProtocolVersions: []int{1}}, nil); err != nil {
		t.Fatal(err)
	}
	var res protocol.ConfigureResult
	if err := host.Call(ctx, "configure", protocol.ConfigureParams{KV: protocol.KVState{Stored: 1, Declared: 2}}, &res); err != nil || res.KVVersion != 2 {
		t.Fatalf("configure: %+v %v", res, err)
	}
	if len(order) != 2 || order[0] != "migrate" {
		t.Fatalf("order %v", order)
	}
}

// TestRequestBeforeServerIsBuilt: a host that writes as soon as the process
// starts (stdin, with nothing ordering it after NewServer) is served by a
// complete server. An operation handler reaches the host from its first
// request, rather than meeting a nil Host and taking the process down; under
// -race, a handler that ran before NewServer had set the server up is
// reported as a race.
func TestRequestBeforeServerIsBuilt(t *testing.T) {
	ext := &sdk.Extension{Manifest: manifest(), Operations: []extension.Operation{{Name: "chat.secret", Handler: func(ctx context.Context, a extension.Authority, _ domain.Payload) (domain.Payload, error) {
		h := sdk.HostFrom(ctx)
		if h == nil {
			return domain.Payload{}, errors.New("no host while serving")
		}
		if _, err := h.ConnectionSecrets(ctx, a.ScopeID, a.Connection, "token"); err != nil {
			return domain.Payload{}, err
		}
		return domain.Payload{Schema: "chat.secret.v1", Data: []byte(`{}`)}, nil
	}}}}
	sr, hw := io.Pipe()
	hr, sw := io.Pipe()
	host := jsonrpc.NewConn(hr, hw, func(context.Context, string, json.RawMessage) (any, error) {
		return protocol.SecretResolveResult{Values: map[string]string{"token": "t"}}, nil
	}, jsonrpc.Options{})
	t.Cleanup(func() { _ = host.Close() })
	// The calls start before NewServer: nothing orders the server's setup
	// before the requests reach it, as with a real process's stdin.
	called := make(chan error, 1)
	go func() {
		ctx := context.Background()
		if err := host.Call(ctx, "initialize", protocol.InitializeParams{ProtocolVersions: []int{1}}, nil); err != nil {
			called <- err
			return
		}
		called <- host.Call(ctx, "operation.call", protocol.OperationParams{Operation: "chat.secret", Authority: protocol.FromAuthority(extension.Authority{ScopeID: "scope", Connection: "bot"}), Input: protocol.FromPayload(domain.Payload{Schema: "chat.secret.input.v1", Data: []byte(`{}`)})}, nil)
	}()
	sdk.NewServer(ext, sr, sw)
	select {
	case err := <-called:
		if err != nil {
			t.Fatalf("first requests: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server never answered")
	}
}
