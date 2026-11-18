package sdk_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// siteProvider runs a command at its acquisition's site, and later at a
// site it resolves through the host.
type siteProvider struct {
	extension.ResourceProvider
	mu    sync.Mutex
	sites extension.SiteResolver
}

func (p *siteProvider) UseSites(r extension.SiteResolver) {
	p.mu.Lock()
	p.sites = r
	p.mu.Unlock()
}
func (*siteProvider) NeedsSite(raw json.RawMessage) bool {
	return strings.Contains(string(raw), "remote")
}

func (p *siteProvider) Acquire(ctx context.Context, a extension.ResourceAcquisition) (extension.Resource, error) {
	if a.Site == nil {
		return extension.Resource{Ref: "on-host"}, nil
	}
	res, err := a.Site.Run(ctx, extension.SiteCommand{Argv: []string{"make"}})
	if err != nil {
		return extension.Resource{}, err
	}
	p.mu.Lock()
	resolve := p.sites
	p.mu.Unlock()
	later, err := resolve(ctx, "pod/b")
	if err != nil {
		return extension.Resource{}, err
	}
	if _, err := later.Run(ctx, extension.SiteCommand{Argv: []string{"rm"}}); err != nil {
		return extension.Resource{}, err
	}
	return extension.Resource{Ref: string(res.Stdout), Site: a.Site.ID()}, nil
}

// TestSitesOverTheWire: a provider declared with sites gets its
// acquisition's site, whose commands cross as site.run quoting the
// host's call, and resolves later sites through the host by ID; config
// check's resource_site is answered from NeedsSite.
func TestSitesOverTheWire(t *testing.T) {
	ext := &sdk.Extension{Manifest: manifest(), Providers: map[string]extension.ResourceProvider{"chat-ws": &siteProvider{}}}
	sr, hw := io.Pipe()
	hr, sw := io.Pipe()
	sdk.NewServer(ext, sr, sw)
	var mu sync.Mutex
	var runs []protocol.SiteRunParams
	host := jsonrpc.NewConn(hr, hw, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		if method != "site.run" {
			return struct{}{}, nil
		}
		var p protocol.SiteRunParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		mu.Lock()
		runs = append(runs, p)
		mu.Unlock()
		return protocol.SiteResult{Stdout: []byte("made")}, nil
	}, jsonrpc.Options{})
	t.Cleanup(func() { _ = host.Close() })
	ctx := context.Background()
	if err := host.Call(ctx, "initialize", protocol.InitializeParams{ProtocolVersions: []int{1}}, nil); err != nil {
		t.Fatal(err)
	}
	var out protocol.ResourceResult
	if err := host.Call(ctx, "provider.acquire", protocol.ProviderAcquireParams{Type: "chat-ws", Site: &protocol.AcquireSite{ID: "pod/a", Call: "c1"}}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Resource.Ref != "made" || out.Resource.Site != "pod/a" {
		t.Fatalf("resource %+v", out.Resource)
	}
	if len(runs) != 2 || runs[0].Call != "c1" || runs[0].Site != "pod/a" || runs[0].Type != "chat-ws" || runs[0].Command.Argv[0] != "make" ||
		runs[1].Call != "" || runs[1].Site != "pod/b" || runs[1].Command.Argv[0] != "rm" {
		t.Fatalf("site.run calls %+v", runs)
	}
	// Without a site, the provider is given none.
	if err := host.Call(ctx, "provider.acquire", protocol.ProviderAcquireParams{Type: "chat-ws"}, &out); err != nil || out.Resource.Ref != "on-host" {
		t.Fatalf("acquire without a site: %+v %v", out.Resource, err)
	}
	for config, want := range map[string]bool{`{"remote":true}`: true, `{}`: false} {
		var v protocol.ValidateResult
		if err := host.Call(ctx, "validate", protocol.ValidateParams{Kind: protocol.ValidateResourceSite, Type: "chat-ws", Config: json.RawMessage(config)}, &v); err != nil || v.Site != want {
			t.Errorf("resource_site %s: %+v %v, want %v", config, v, err, want)
		}
	}
}
