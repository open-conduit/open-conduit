// Package goplugin is the prototype go-plugin (gRPC) transport. Without protoc
// in the toolchain the services are hand-written grpc.ServiceDescs whose
// messages are protobuf BytesValue wrappers carrying the same JSON envelopes
// as the JSON-RPC prototype; a real implementation would generate typed
// messages. Extension-to-host calls use go-plugin's GRPCBroker, which opens a
// second multiplexed stream back to a host-served gRPC service.
package goplugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/alsiola/open-conduit/prototypes/transport/scenario"
)

const hostBrokerID = 1

func handshake(version int) plugin.HandshakeConfig {
	return plugin.HandshakeConfig{ProtocolVersion: uint(version), MagicCookieKey: "OPEN_CONDUIT_PROTOTYPE", MagicCookieValue: "07-01"}
}

type envelope struct {
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *scenario.Error `json:"error,omitempty"`
}

type caller interface {
	Call(context.Context, *wrapperspb.BytesValue) (*wrapperspb.BytesValue, error)
}

func desc(service string) *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: service,
		HandlerType: (*caller)(nil),
		Methods: []grpc.MethodDesc{{MethodName: "Call", Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			in := new(wrapperspb.BytesValue)
			if err := dec(in); err != nil {
				return nil, err
			}
			call := func(ctx context.Context, req any) (any, error) {
				return srv.(caller).Call(ctx, req.(*wrapperspb.BytesValue))
			}
			if interceptor == nil {
				return call(ctx, in)
			}
			return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + service + "/Call"}, call)
		}}},
		Metadata: "prototype",
	}
}

// server adapts a JSON handler to the hand-written service.
type server struct {
	handle func(ctx context.Context, method string, params json.RawMessage) (any, error)
}

func (s server) Call(ctx context.Context, in *wrapperspb.BytesValue) (*wrapperspb.BytesValue, error) {
	var req envelope
	if err := json.Unmarshal(in.Value, &req); err != nil {
		return nil, status.Error(codes.InvalidArgument, "bad envelope")
	}
	result, err := s.handle(ctx, req.Method, req.Params)
	var out envelope
	var se *scenario.Error
	switch {
	case err == nil:
		if out.Result, err = json.Marshal(result); err != nil {
			return nil, status.Error(codes.Internal, "unencodable result")
		}
	case ctx.Err() != nil:
		return nil, status.Error(codes.Canceled, "request cancelled")
	case errors.As(err, &se):
		out.Error = se
	default:
		return nil, status.Error(codes.Internal, "internal error")
	}
	data, _ := json.Marshal(out)
	return wrapperspb.Bytes(data), nil
}

func invoke(ctx context.Context, conn *grpc.ClientConn, service, method string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	data, _ := json.Marshal(envelope{Method: method, Params: raw})
	out := new(wrapperspb.BytesValue)
	if err := conn.Invoke(ctx, "/"+service+"/Call", wrapperspb.Bytes(data), out); err != nil {
		switch status.Code(err) {
		case codes.Canceled:
			return nil, context.Canceled
		case codes.DeadlineExceeded:
			return nil, context.DeadlineExceeded
		case codes.Unavailable:
			return nil, scenario.ErrTransportClosed
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &scenario.Error{Code: "internal", Message: status.Convert(err).Message()}
	}
	var resp envelope
	if err := json.Unmarshal(out.Value, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

// extensionPlugin is the go-plugin registration on both sides.
type extensionPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	name   string
	handle func(ctx context.Context, method string, params json.RawMessage, host scenario.Caller) (any, error)
	host   scenario.HostHandler
}

// GRPCServer runs in the extension: it serves the extension service and
// dials the host service through the broker on first use.
func (p *extensionPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	var once sync.Once
	var hostConn *grpc.ClientConn
	var dialErr error
	host := func(ctx context.Context, method string, params any) (json.RawMessage, error) {
		once.Do(func() { hostConn, dialErr = broker.Dial(hostBrokerID) })
		if dialErr != nil {
			return nil, scenario.ErrTransportClosed
		}
		return invoke(ctx, hostConn, "oc.prototype.Host", method, params)
	}
	s.RegisterService(desc("oc.prototype.Extension"), server{handle: func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		if method == "describe" {
			return scenario.Manifest{Name: p.name, Protocol: scenario.SupportedVersions()[0], Provides: []string{"echo", "kv", "artifact", "sleep", "push"}}, nil
		}
		return p.handle(ctx, method, params, host)
	}})
	return nil
}

// GRPCClient runs in the host: it serves host callbacks on the broker and
// returns a client for the extension service.
func (p *extensionPlugin) GRPCClient(_ context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	go broker.AcceptAndServe(hostBrokerID, func(opts []grpc.ServerOption) *grpc.Server {
		s := grpc.NewServer(opts...)
		s.RegisterService(desc("oc.prototype.Host"), server{handle: p.host})
		return s
	})
	return c, nil
}

// Serve runs an extension under go-plugin.
func Serve(name string, h func(ctx context.Context, method string, params json.RawMessage, host scenario.Caller) (any, error)) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: handshake(scenario.SupportedVersions()[0]),
		Plugins:         map[string]plugin.Plugin{"extension": &extensionPlugin{name: name, handle: h}},
		GRPCServer:      plugin.DefaultGRPCServer,
		Logger:          hclog.New(&hclog.LoggerOptions{Level: hclog.Error, Output: os.Stderr}),
	})
}

type peer struct {
	client   *plugin.Client
	conn     *grpc.ClientConn
	manifest json.RawMessage
}

// Start launches binary under go-plugin. go-plugin's handshake supports one
// protocol version per host config, so versions is tried highest first.
func Start(ctx context.Context, binary string, env []string, versions []int, host scenario.HostHandler) (scenario.Peer, error) {
	var lastErr error
	for i := len(versions) - 1; i >= 0; i-- {
		cmd := exec.Command(binary)
		cmd.Env = append(os.Environ(), env...)
		client := plugin.NewClient(&plugin.ClientConfig{
			HandshakeConfig:  handshake(versions[i]),
			Plugins:          map[string]plugin.Plugin{"extension": &extensionPlugin{host: host}},
			Cmd:              cmd,
			AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
			Logger:           hclog.New(&hclog.LoggerOptions{Level: hclog.Off, Output: io.Discard}),
			Stderr:           os.Stderr,
		})
		rpc, err := client.Client()
		if err != nil {
			client.Kill()
			lastErr = err
			continue
		}
		raw, err := rpc.Dispense("extension")
		if err != nil {
			client.Kill()
			return nil, err
		}
		p := &peer{client: client, conn: raw.(*grpc.ClientConn)}
		if p.manifest, err = p.Call(ctx, "describe", nil); err != nil {
			client.Kill()
			return nil, err
		}
		return p, nil
	}
	if lastErr != nil {
		return nil, errors.Join(scenario.ErrIncompatible, lastErr)
	}
	return nil, scenario.ErrIncompatible
}

func (p *peer) Manifest() json.RawMessage { return p.manifest }
func (p *peer) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if p.client.Exited() {
		return nil, scenario.ErrTransportClosed
	}
	return invoke(ctx, p.conn, "oc.prototype.Extension", method, params)
}
func (p *peer) Close() error { p.client.Kill(); return nil }
