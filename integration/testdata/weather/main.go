// Command weather-extension is the package TestExtensiontestPackage tests
// through extensiontest: a forecast operation that calls the weather
// service its entry names with the scope's api_key secret, and a report
// operation that records an Event.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

var manifest = protocol.Manifest{Protocol: []int{protocol.Version}, Name: "weather", Version: "0.1.0", Summary: "Forecasts for agents.", Command: []string{"weather-extension"},
	Provides: protocol.Provides{
		Extensions: []protocol.ExtensionDecl{{Type: "weather"}},
		Families:   []protocol.Family{{Type: "weather.reported", ReDerivable: true}},
		Schemas:    []protocol.SchemaDecl{{Name: "weather.report", Versions: []int{1}}},
		Operations: []protocol.OperationDecl{{Name: "weather.forecast", Kind: protocol.OperationHandler}, {Name: "weather.report", Kind: protocol.OperationEmit, Commit: true}},
	}}

func main() {
	if sdk.PackageCommand(os.Args[1:], manifest, nil, os.Stdout) {
		return
	}
	var mu sync.Mutex
	services := map[string]string{}
	ext := &sdk.Extension{Manifest: manifest}
	ext.Configure = func(_ context.Context, _ *sdk.Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
		next := map[string]string{}
		for _, e := range p.Entries {
			var cfg struct {
				Service string `json:"service"`
			}
			_ = json.Unmarshal(e.Config, &cfg)
			next[e.ScopeID] = cfg.Service
		}
		mu.Lock()
		services = next
		mu.Unlock()
		return protocol.ConfigureResult{}, nil
	}
	ext.Operations = []extension.Operation{
		{Name: "weather.forecast", Handler: func(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
			var req struct {
				City string `json:"city"`
			}
			if json.Unmarshal(in.Data, &req) != nil || req.City == "" {
				return domain.Payload{}, &extension.OperationError{Code: "bad_request", Message: "a forecast needs a city"}
			}
			mu.Lock()
			service := services[string(a.ScopeID)]
			mu.Unlock()
			secrets, err := sdk.HostFrom(ctx).Secrets(ctx, a.ScopeID, "api_key")
			if err != nil {
				return domain.Payload{}, err
			}
			r, err := http.NewRequestWithContext(ctx, "GET", service+"?city="+url.QueryEscape(req.City), nil)
			if err != nil {
				return domain.Payload{}, err
			}
			r.Header.Set("Authorization", "Bearer "+secrets["api_key"])
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				return domain.Payload{}, err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				return domain.Payload{}, &extension.OperationError{Code: "service_refused", Message: fmt.Sprintf("the weather service answered %d", resp.StatusCode)}
			}
			out, _ := json.Marshal(map[string]string{"forecast": string(body)})
			return domain.Payload{Schema: "weather.forecast.v1", Data: out}, nil
		}},
		{Name: "weather.report", Emit: func(ctx context.Context, _ extension.Authority, in domain.Payload) (extension.EmitPlan, error) {
			return extension.EmitPlan{Event: domain.Event{Type: "weather.reported", DeliveryID: "report/" + extension.CallRequestID(ctx), Data: domain.Payload{Schema: "weather.report.v1", Data: in.Data}},
				Commit: func(em extension.Emitted) (extension.EmitCommit, error) {
					out, _ := json.Marshal(map[string]string{"event": string(em.Event.Event.ID)})
					return extension.EmitCommit{Output: domain.Payload{Schema: "weather.report.v1", Data: out}}, nil
				}}, nil
		}},
	}
	out := os.Stdout
	os.Stdout = os.Stderr
	<-sdk.NewServer(ext, os.Stdin, out).Done()
}
