// Package mcpext packages the MCP bridge (extensions/mcp) as an
// installable subprocess extension served over protocol version 1 through
// protocol/sdk. Nothing in the daemon knows about MCP: the bridge's two
// entry types declare their tools as entry operations, and an mcp-stdio
// entry's server launches through sandbox.entry_plan in the sandbox the
// entry names.
package mcpext

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/mcp"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// Name is the package's registration name.
const Name = mcp.Name

// Version is this package build's own version.
const Version = "0.1.0"

// markAge is how long a mark of a call sent is kept.
const markAge = 7 * 24 * time.Hour

func fields(stdio bool) []protocol.FieldDecl {
	tools := protocol.FieldDecl{Name: "tools", Required: true, Help: "The tools taken from the server, each with name, its description and input_schema exactly as the server lists them, and optionally operation, summary, toolset (default tools) and retry_safe (default false)."}
	timeout := protocol.FieldDecl{Name: "timeout_seconds", Default: "60", Help: "How long one call, or the server's start, may take."}
	if stdio {
		return []protocol.FieldDecl{
			{Name: "command", Required: true, Help: "The server's argv, run in the entry's sandbox."},
			{Name: "env", Help: "Variables the server sees, besides PATH, HOME and LANG."},
			{Name: "secret_env", Help: "Variables set from the entry's secrets: variable to secret name."},
			timeout, tools,
		}
	}
	return []protocol.FieldDecl{
		{Name: "url", Required: true, Help: "The server's streamable HTTP endpoint (https)."},
		{Name: "headers", Help: "Headers sent with every request."},
		{Name: "secret_headers", Help: "Headers set from the entry's secrets: header to secret name."},
		{Name: "bearer_secret", Help: "A secret the entry selects, sent as the bearer token."},
		timeout, tools,
	}
}

// Build returns the served extension around a bridge.
func Build(b *mcp.Bridge) *sdk.Extension {
	ext := sdk.New(Name, sdk.Version(Version), sdk.Summary("Bridges the tools an operator lists from MCP servers into toolbelt operations."))
	ext.Manifest.Title = "MCP bridge"
	ext.Manifest.Command = protocol.HostRunCommand(Name)
	serve := func(ctx context.Context, h *sdk.Host, c sdk.EntryCall) (domain.Payload, error) {
		return b.Serve(ctx, h, mcp.Call{Type: c.Type, Entry: c.Entry, Scope: c.Authority.ScopeID, Config: c.Config,
			Operation: c.Operation.Name, RequestID: extension.CallRequestID(ctx), Input: c.Input})
	}
	for _, t := range []struct {
		typ, sandbox string
	}{{mcp.StdioType, protocol.EntrySandboxRequired}, {mcp.HTTPType, ""}} {
		typ := t.typ
		ext.EntryOperations(typ, sdk.EntryType{Sandbox: t.sandbox, Serve: serve,
			Declared: func(raw json.RawMessage) ([]extension.EntryOperationDecl, error) { return mcp.Declared(typ, raw) }})
	}
	for i := range ext.Manifest.Provides.Extensions {
		ext.Manifest.Provides.Extensions[i].Fields = fields(ext.Manifest.Provides.Extensions[i].Type == mcp.StdioType)
	}
	ext.Validate = func(_ context.Context, p protocol.ValidateParams) error {
		if p.Kind != protocol.ValidateExtension || p.Type != mcp.StdioType && p.Type != mcp.HTTPType {
			return nil
		}
		return mcp.Validate(p.Type, p.Config, p.SecretNames)
	}
	ext.Configure = func(_ context.Context, _ *sdk.Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
		var entries []mcp.ConfiguredEntry
		for _, e := range p.Entries {
			entries = append(entries, mcp.ConfiguredEntry{Scope: domain.ID(e.ScopeID), Name: e.Name, Type: e.Type, Config: e.Config})
		}
		b.Configure(entries)
		return protocol.ConfigureResult{}, nil
	}
	ext.Task("mcp-sweep-calls", time.Hour, func(ctx context.Context, h *sdk.Host) error {
		return mcp.Sweep(ctx, h, time.Now(), markAge)
	})
	return ext
}

// Manifest is the package's manifest.
func Manifest() protocol.Manifest { return Build(mcp.New(mcp.Options{})).Manifest }

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run mcp` does; package-manifest and
// package-certification print the package files. It returns the process's
// exit status.
func Main(args []string) int {
	if sdk.PackageCommand(args, Manifest(), nil, os.Stdout) {
		return 0
	}
	b := mcp.New(mcp.Options{})
	defer b.Close()
	out := os.Stdout
	os.Stdout = os.Stderr
	<-sdk.NewServer(Build(b), os.Stdin, out).Done()
	return 0
}
