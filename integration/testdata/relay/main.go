// Command relay-extension is the package TestExtensiontestForwardedWebhook
// tests through extensiontest: a third-party package with one webhook path
// per entry, /relay/<scope>, that answers with the receipt the host
// attested for the request (extension.WebhookReceivedAt and
// WebhookForwardID) and the headers it was given, so the test sees what a
// package sees behind a forwarder.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

var manifest = protocol.Manifest{Protocol: []int{protocol.Version}, Name: "relay", Version: "0.1.0", Summary: "Echoes a webhook request's receipt.", Command: []string{"relay-extension"},
	Provides: protocol.Provides{
		Extensions: []protocol.ExtensionDecl{{Type: "relay", Ingress: true}},
		Families:   []protocol.Family{{Type: "relay.received", Loss: "A test package: nothing is delivered."}},
		Webhooks:   true,
	}}

// Echo is the body of the relay's answer.
type Echo struct {
	ReceivedAt time.Time `json:"received_at"`
	ForwardID  string    `json:"forward_id"`
	Headers    []string  `json:"headers"`
}

func main() {
	if sdk.PackageCommand(os.Args[1:], manifest, nil, os.Stdout) {
		return
	}
	var mu sync.Mutex
	paths := map[string]bool{}
	ext := &sdk.Extension{Manifest: manifest}
	ext.Configure = func(_ context.Context, _ *sdk.Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
		var out protocol.ConfigureResult
		next := map[string]bool{}
		for _, e := range p.Entries {
			next[e.ScopeID] = true
			out.WebhookPaths = append(out.WebhookPaths, "/relay/"+e.ScopeID)
		}
		mu.Lock()
		paths = next
		mu.Unlock()
		return out, nil
	}
	ext.Webhooks = func(path string) (http.Handler, bool) {
		mu.Lock()
		ok := paths[strings.TrimPrefix(path, "/relay/")]
		mu.Unlock()
		if !ok {
			return nil, false
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var names []string
			for k := range r.Header {
				names = append(names, k)
			}
			sort.Strings(names)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Echo{ReceivedAt: extension.WebhookReceivedAt(r, time.Now), ForwardID: extension.WebhookForwardID(r), Headers: names})
		}), true
	}
	out := os.Stdout
	os.Stdout = os.Stderr
	<-sdk.NewServer(ext, os.Stdin, out).Done()
}
