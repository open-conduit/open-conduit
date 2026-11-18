package protocol_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
)

// What a workspace at a launch's site adds to the executor's surfaces
// crosses the wire: the resource's site and the files read there, the
// files an executor asks for, the plan's wrapper directory and the
// policy's site.
func TestSiteFieldsCrossTheWire(t *testing.T) {
	roundTrip := func(in, out any) {
		t.Helper()
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
	}
	res := extension.Resource{Kind: "workspace", Ref: "r", Dir: "/w", Site: "ssh/runner", Files: map[string]string{"a.md": "x"}}
	var wr protocol.Resource
	roundTrip(protocol.FromResource(res), &wr)
	if got := wr.Extension(); got.Site != res.Site || !reflect.DeepEqual(got.Files, res.Files) {
		t.Fatalf("resource %+v, want site and files", got)
	}
	plan := extension.SandboxPlan{Argv: []string{"x"}, Dir: "/run"}
	var wp protocol.SandboxPlan
	roundTrip(protocol.FromSandboxPlan(plan), &wp)
	if wp.Extension().Dir != "/run" {
		t.Fatalf("plan %+v, want its dir", wp)
	}
	paths := extension.SandboxPaths{Workspace: "/w", Site: "ssh/runner"}
	var wpa protocol.SandboxPaths
	roundTrip(protocol.FromSandboxPaths(paths), &wpa)
	if wpa.Extension() != paths {
		t.Fatalf("paths %+v, want %+v", wpa.Extension(), paths)
	}
	var acq protocol.ResourceAcquireParams
	roundTrip(protocol.ResourceAcquireParams{RunID: "r", Files: []string{"a.md"}}, &acq)
	var prov protocol.ProviderAcquireParams
	roundTrip(protocol.ProviderAcquireParams{Type: "git", Files: []string{"a.md"}}, &prov)
	if !reflect.DeepEqual(acq.Files, []string{"a.md"}) || !reflect.DeepEqual(prov.Files, []string{"a.md"}) {
		t.Fatalf("files %v %v", acq.Files, prov.Files)
	}
}
