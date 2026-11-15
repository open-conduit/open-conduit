// Command commands-extension is the package TestEntryOperationsPackage
// tests through extensiontest: an entry type whose config declares the
// toolbelt operations it offers (entry operations), each answering with a
// greeting. The entry's "live" list stands in for what a real entry would
// front (an MCP server's tools, a script): an operation the config declares
// but "live" no longer lists has drifted, and its calls are refused.
package main

import (
	"context"
	"encoding/json"
	"os"
	"slices"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

type config struct {
	Operations []extension.EntryOperationDecl `json:"operations"`
	Live       []string                       `json:"live"`
}

func main() {
	ext := sdk.New("commands", sdk.Version("0.1.0"), sdk.Summary("Operations an entry's config declares."))
	ext.EntryOperations("commands", sdk.EntryType{
		Declared: func(raw json.RawMessage) ([]extension.EntryOperationDecl, error) {
			var c config
			if err := json.Unmarshal(raw, &c); err != nil {
				return nil, err
			}
			return c.Operations, nil
		},
		Serve: func(_ context.Context, _ *sdk.Host, c sdk.EntryCall) (domain.Payload, error) {
			var cfg config
			_ = json.Unmarshal(c.Config, &cfg)
			if !slices.Contains(cfg.Live, c.Operation.Name) {
				return domain.Payload{}, &extension.OperationError{Code: "drifted", Message: c.Operation.Name + " has drifted from what the entry declares; re-apply the config"}
			}
			var in struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(c.Input.Data, &in)
			out, _ := json.Marshal(map[string]string{"text": "hello " + in.Name + " from " + c.Entry + "." + c.Operation.Name})
			return domain.Payload{Schema: "commands.greeting.v1", Data: out}, nil
		},
	})
	if sdk.PackageCommand(os.Args[1:], ext.Manifest, nil, os.Stdout) {
		return
	}
	out := os.Stdout
	os.Stdout = os.Stderr
	<-sdk.NewServer(ext, os.Stdin, out).Done()
}
