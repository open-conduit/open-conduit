package extension_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// credential is what a fake provider's package resolved for its
// connection: it has a quote in it, so the plan's JSON escapes it.
const credential = `tok-"account-a"-0123456789`

// leakingContainers is a provider that puts the credential it resolved
// somewhere that reaches the run, its history or the container.
type leakingContainers struct {
	*fakeContainers
	into string
}

func (l *leakingContainers) Plan(ctx context.Context, o extension.ResourceOwner, p extension.ContainerPolicy, n extension.SandboxNeeds, sl extension.SandboxLaunch) (extension.ContainerPlan, error) {
	plan, err := l.fakeContainers.Plan(ctx, o, p, n, sl)
	switch l.into {
	case "summary":
		plan.Summary += " token " + credential
	case "data":
		raw, _ := json.Marshal(map[string]string{"auth": credential})
		plan.Data = domain.Payload{Schema: "fake.v1", Data: raw}
	}
	return plan, err
}

func (l *leakingContainers) Create(ctx context.Context, p extension.ContainerPlan, labels map[string]string) (extension.ContainerCreated, error) {
	c, err := l.fakeContainers.Create(ctx, p, labels)
	switch l.into {
	case "env":
		c.Env = append(c.Env, "CLOUD_TOKEN="+credential)
	case "argv":
		c.Argv = append(c.Argv, "--token", credential)
	case "label":
		c.Ref.Labels = map[string]string{"cloud.token": credential}
	}
	return c, err
}

// TestContainerLaunchWithholdsCredentials: a credential the provider's
// package resolved for its connection never reaches the run. A launch
// whose plan (summary or data), environment, command line or labels carry
// it is refused, its container removed and nothing pinned; the refusal
// doesn't quote it. A provider that keeps it to itself launches.
func TestContainerLaunchWithholdsCredentials(t *testing.T) {
	cfg := containerConfig(testImage, `,"connection":"cloud-a"`)
	withheld := func() []string { return []string{"short", credential} }
	bind := func(h *containerHost, p extension.ContainerProvider, withheld func() []string) *extension.BoundContainer {
		return extension.BindContainer(extension.ContainerBinding{Provider: p, Owner: extension.ResourceOwner{ScopeID: "s", WorkID: "w", ExecutionID: "x"},
			Config: cfg, Host: "host-a", Segment: 1, Protected: []string{h.state}, Shared: []string{filepath.Dir(h.toolbelt)}, RunRoot: h.runRoot,
			Toolbelt: h.toolbelt, Records: h.records, Home: h.home, Now: func() time.Time { return h.now }, Withheld: withheld,
			Pin: func(p extension.ContainerPin) error { h.pins = append(h.pins, p); return nil }})
	}
	for _, into := range []string{"summary", "data", "env", "argv", "label"} {
		t.Run(into, func(t *testing.T) {
			h := newContainerHost(t)
			f := &leakingContainers{fakeContainers: newFakeContainers(), into: into}
			_, err := bind(h, f, withheld).Launch(context.Background(), h.needs(), h.launch())
			if !errors.Is(err, extension.ErrSandboxDenied) {
				t.Fatalf("err %v, want sandbox denied", err)
			}
			if strings.Contains(err.Error(), "0123456789") {
				t.Fatalf("the refusal quotes the credential: %v", err)
			}
			if len(h.pins) != 0 || len(f.containers) != 0 {
				t.Fatalf("a refused launch kept a container (%v) or pinned one (%v)", f.containers, h.pins)
			}
			// Negative control: without the credentials withheld the
			// same provider launches with the credential in it.
			h = newContainerHost(t)
			f = &leakingContainers{fakeContainers: newFakeContainers(), into: into}
			if _, err := bind(h, f, nil).Launch(context.Background(), h.needs(), h.launch()); err != nil {
				t.Fatalf("without withheld credentials: %v", err)
			}
		})
	}
	h := newContainerHost(t)
	f := newFakeContainers()
	p, err := bind(h, f, withheld).Launch(context.Background(), h.needs(), h.launch())
	if err != nil {
		t.Fatalf("a provider keeping its credential: %v", err)
	}
	if f.planned.policy.Config.Data == nil || !strings.Contains(string(f.planned.policy.Config.Data), `"connection":"cloud-a"`) {
		t.Fatalf("the provider wasn't given its connection: %s", f.planned.policy.Config.Data)
	}
	for _, s := range append(append(p.Argv, p.Env...), p.Summary, string(p.Data.Data)) {
		if strings.Contains(s, credential) {
			t.Fatalf("the launch carries the credential: %+v", p)
		}
	}
}
