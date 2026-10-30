package extension_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

const testImage = "ghcr.io/acme/agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeContainers is a container provider kept in memory, so the contract
// is tested without a container daemon.
type fakeContainers struct {
	daemon     string
	containers map[string]*extension.ContainerState
	listErr    error
	inspectErr error
	created    []map[string]string
	removed    []string
	stopped    []string
	// stopFails leaves a container running after Stop.
	stopFails bool
	planned   struct {
		needs  extension.SandboxNeeds
		policy extension.ContainerPolicy
	}
	n int
}

func newFakeContainers() *fakeContainers {
	return &fakeContainers{daemon: "daemon-1", containers: map[string]*extension.ContainerState{}}
}

func (f *fakeContainers) Plan(_ context.Context, _ extension.ResourceOwner, p extension.ContainerPolicy, n extension.SandboxNeeds, _ extension.SandboxLaunch) (extension.ContainerPlan, error) {
	f.planned.needs, f.planned.policy = n, p
	cfg, err := extension.ParseContainerConfig(p.Config.Data)
	if err != nil {
		return extension.ContainerPlan{}, err
	}
	return extension.ContainerPlan{Image: cfg.Image, Summary: "fake " + cfg.Image[:10], Data: domain.Payload{Schema: "fake.v1", Data: json.RawMessage(`{"mounts":1}`)}}, nil
}

func (f *fakeContainers) Create(_ context.Context, p extension.ContainerPlan, labels map[string]string) (extension.ContainerCreated, error) {
	f.n++
	id := "c" + string(rune('0'+f.n))
	f.created = append(f.created, labels)
	f.containers[id] = &extension.ContainerState{Ref: extension.ContainerRef{ID: id, Labels: labels}, Daemon: f.daemon, Image: p.Image}
	return extension.ContainerCreated{Ref: extension.ContainerRef{ID: id, Labels: labels}, Daemon: f.daemon, Argv: []string{"open-conduit", "container", "run", id}}, nil
}

// start is what the wrapper's helper does: it starts the container.
func (f *fakeContainers) start(id string, at time.Time) {
	f.containers[id].Running, f.containers[id].StartedAt = true, at
}

func (f *fakeContainers) Signal(context.Context, extension.ContainerRef, string) error { return nil }

func (f *fakeContainers) Stop(_ context.Context, ref extension.ContainerRef, _ time.Duration) error {
	f.stopped = append(f.stopped, ref.ID)
	if c := f.containers[ref.ID]; c != nil && !f.stopFails {
		c.Running, c.ExitCode = false, 143
	}
	return nil
}

func (f *fakeContainers) Inspect(_ context.Context, ref extension.ContainerRef) (extension.ContainerState, error) {
	if f.inspectErr != nil {
		return extension.ContainerState{}, f.inspectErr
	}
	c := f.containers[ref.ID]
	if c == nil {
		return extension.ContainerState{Ref: ref, Daemon: f.daemon, Gone: true}, nil
	}
	return *c, nil
}

func (f *fakeContainers) List(_ context.Context, o extension.ResourceOwner) ([]extension.ContainerState, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []extension.ContainerState
	for _, c := range f.containers {
		if o.ExecutionID == "" || c.Ref.Labels[extension.ContainerLabelExecution] == string(o.ExecutionID) {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (f *fakeContainers) Remove(_ context.Context, ref extension.ContainerRef) error {
	f.removed = append(f.removed, ref.ID)
	delete(f.containers, ref.ID)
	return nil
}

type containerHost struct {
	*sandboxHost
	home string
	pins []extension.ContainerPin
	now  time.Time
}

func newContainerHost(t *testing.T) *containerHost {
	h := &containerHost{sandboxHost: newSandboxHost(t), now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	h.records = filepath.Join(h.state, "runs", "k", "container")
	h.home = filepath.Join(filepath.Dir(h.state), "home")
	_ = os.MkdirAll(h.home, 0o700)
	return h
}

func containerConfig(image string, extra string) domain.Payload {
	return domain.Payload{Schema: extension.ContainerConfigSchema, Data: json.RawMessage(`{"image":"` + image + `","write":["{workspace}"],"read":["~/.gitconfig"],"network":{"allow":["api.anthropic.com","*.github.com"]}` + extra + `}`)}
}

func (h *containerHost) bind(p extension.ContainerProvider, cfg domain.Payload, pinned *extension.ContainerPin) *extension.BoundContainer {
	return extension.BindContainer(extension.ContainerBinding{Provider: p, Owner: extension.ResourceOwner{ScopeID: "s", WorkID: "w", ExecutionID: "x"},
		Config: cfg, Host: "host-a", Segment: 1, Protected: []string{h.state}, Shared: []string{filepath.Dir(h.toolbelt)}, RunRoot: h.runRoot,
		Toolbelt: h.toolbelt, Records: h.records, Home: h.home, Pinned: pinned, Now: func() time.Time { return h.now },
		Pin: func(p extension.ContainerPin) error { h.pins = append(h.pins, p); return nil }})
}

func (h *containerHost) needs() extension.SandboxNeeds {
	return extension.SandboxNeeds{Write: []string{h.ws}, Read: []string{filepath.Join(h.home, ".gitconfig")}, Hosts: []string{"api.anthropic.com"}, Sockets: []string{h.toolbelt}}
}

func TestContainerLaunchCreatesAndPins(t *testing.T) {
	h := newContainerHost(t)
	f := newFakeContainers()
	p, err := h.bind(f, containerConfig(testImage, ""), nil).Launch(context.Background(), h.needs(), h.launch())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Argv, " ") != "open-conduit container run c1" || p.Signals != extension.SandboxTerm {
		t.Errorf("plan %+v, want the provider's start argv with term", p)
	}
	if p.Digest != extension.ContainerDigest(testImage, p.Data) {
		t.Errorf("digest %q does not pin the image and policy", p.Digest)
	}
	if len(h.pins) != 1 {
		t.Fatalf("pins %+v", h.pins)
	}
	pin := h.pins[0]
	if pin.ID != "c1" || pin.Image != testImage || pin.Daemon != "daemon-1" || pin.Digest != p.Digest || pin.Run != h.run || !pin.At.Equal(h.now) {
		t.Errorf("pin %+v, want the container, image, daemon and plan", pin)
	}
	l := f.created[0]
	if l[extension.ContainerLabelHost] != "host-a" || l[extension.ContainerLabelExecution] != "x" || l[extension.ContainerLabelSegment] != "1" || l[extension.ContainerLabelDigest] != p.Digest {
		t.Errorf("labels %v", l)
	}
	if f.containers["c1"].Running {
		t.Error("Launch started the container; only the wrapper may")
	}
	if !contains(f.planned.needs.Deny, h.state) || !contains(f.planned.needs.Read, filepath.Dir(h.toolbelt)) {
		t.Errorf("needs %+v, want the protected set and the shared paths merged", f.planned.needs)
	}
}

func TestContainerLaunchRefuses(t *testing.T) {
	cases := []struct {
		name  string
		cfg   domain.Payload
		setup func(h *containerHost, f *fakeContainers, l *extension.SandboxLaunch, n *extension.SandboxNeeds)
		want  error
	}{
		{"image_not_pinned_by_digest", containerConfig("ghcr.io/acme/agent:latest", ""), nil, extension.ErrSandboxUnavailable},
		{"listing_fails", containerConfig(testImage, ""), func(_ *containerHost, f *fakeContainers, _ *extension.SandboxLaunch, _ *extension.SandboxNeeds) {
			f.listErr = errors.New("daemon unreachable")
		}, extension.ErrSandboxUnavailable},
		{"second_container", containerConfig(testImage, ""), func(_ *containerHost, f *fakeContainers, _ *extension.SandboxLaunch, _ *extension.SandboxNeeds) {
			f.containers["old"] = &extension.ContainerState{Ref: extension.ContainerRef{ID: "old", Labels: map[string]string{extension.ContainerLabelExecution: "x"}}, Running: true}
		}, extension.ErrContainerRunning},
		{"write_outside_the_grant", containerConfig(testImage, ""), func(h *containerHost, _ *fakeContainers, _ *extension.SandboxLaunch, n *extension.SandboxNeeds) {
			n.Write = append(n.Write, filepath.Join(h.home, ".ssh"))
		}, extension.ErrSandboxDenied},
		{"read_outside_the_grant", containerConfig(testImage, ""), func(h *containerHost, _ *fakeContainers, _ *extension.SandboxLaunch, n *extension.SandboxNeeds) {
			n.Read = append(n.Read, filepath.Join(h.home, ".ssh", "id_ed25519"))
		}, extension.ErrSandboxDenied},
		{"host_outside_the_grant", containerConfig(testImage, ""), func(_ *containerHost, _ *fakeContainers, _ *extension.SandboxLaunch, n *extension.SandboxNeeds) {
			n.Hosts = append(n.Hosts, "evil.example")
		}, extension.ErrSandboxDenied},
		{"socket_other_than_the_toolbelt", containerConfig(testImage, ""), func(h *containerHost, _ *fakeContainers, _ *extension.SandboxLaunch, n *extension.SandboxNeeds) {
			n.Sockets = append(n.Sockets, filepath.Join(h.home, "docker.sock"))
		}, extension.ErrSandboxDenied},
		{"grant_reaching_the_state", domain.Payload{Data: json.RawMessage(`{"image":"` + testImage + `","write":["{workspace}","{workspace}/../.."]}`)}, nil, extension.ErrSandboxDenied},
		// The shared launch checks (GuardLaunch), as for a process sandbox.
		{"need_inside_the_state", containerConfig(testImage, ""), func(h *containerHost, _ *fakeContainers, _ *extension.SandboxLaunch, n *extension.SandboxNeeds) {
			n.Read = append(n.Read, filepath.Join(h.state, "private", "operator.token"))
		}, extension.ErrSandboxDenied},
		{"hard_link_to_a_protected_file", containerConfig(testImage, ""), func(h *containerHost, _ *fakeContainers, _ *extension.SandboxLaunch, _ *extension.SandboxNeeds) {
			_ = os.Link(filepath.Join(h.state, "private", "operator.token"), filepath.Join(h.ws, "token"))
		}, extension.ErrSandboxDenied},
		{"run_dir_outside_the_executor", containerConfig(testImage, ""), func(h *containerHost, _ *fakeContainers, l *extension.SandboxLaunch, _ *extension.SandboxNeeds) {
			l.RunDir = filepath.Join(h.state, "runs", "k")
		}, extension.ErrSandboxDenied},
		// A mount over a symlink would hold its target, not the link.
		{"read_only_need_is_a_symlink", containerConfig(testImage, ""), func(h *containerHost, _ *fakeContainers, _ *extension.SandboxLaunch, n *extension.SandboxNeeds) {
			_ = os.Symlink(filepath.Join(h.ws, "elsewhere"), filepath.Join(h.ws, "config"))
			n.ReadOnly = append(n.ReadOnly, filepath.Join(h.ws, "config"))
		}, extension.ErrSandboxUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newContainerHost(t)
			f := newFakeContainers()
			l, n := h.launch(), h.needs()
			if c.setup != nil {
				c.setup(h, f, &l, &n)
			}
			_, err := h.bind(f, c.cfg, nil).Launch(context.Background(), n, l)
			if !errors.Is(err, c.want) {
				t.Fatalf("err %v, want %v", err, c.want)
			}
			if len(h.pins) != 0 || len(f.created) != 0 {
				t.Error("a refused launch created or pinned a container")
			}
		})
	}
}

func TestContainerLaunchRemovesStoppedContainers(t *testing.T) {
	h := newContainerHost(t)
	f := newFakeContainers()
	f.containers["old"] = &extension.ContainerState{Ref: extension.ContainerRef{ID: "old", Labels: map[string]string{extension.ContainerLabelExecution: "x"}}, ExitCode: 1}
	if _, err := h.bind(f, containerConfig(testImage, ""), nil).Launch(context.Background(), h.needs(), h.launch()); err != nil {
		t.Fatal(err)
	}
	if !contains(f.removed, "old") {
		t.Errorf("removed %v, want the stopped container removed before the launch", f.removed)
	}
}

func TestContainerReason(t *testing.T) {
	if extension.ContainerReason(extension.ErrContainerRunning) != "container_running" || extension.ContainerReason(extension.ErrSandboxDenied) != "sandbox_denied" ||
		extension.ContainerReason(errors.New("x")) != "sandbox_unavailable" {
		t.Fatal("reasons")
	}
}

func TestParseContainerConfig(t *testing.T) {
	for _, bad := range []string{
		`{"image":"alpine"}`,
		`{"image":"alpine:3.20"}`,
		`{"image":"alpine@sha256:abc"}`,
		`{"image":"` + testImage + `","write":["relative"]}`,
		`{"image":"` + testImage + `","network":{"allow":["http://x"]}}`,
		`{"image":"` + testImage + `","limits":{"cpus":-1}}`,
		`{"image":"` + testImage + `","unknown":1}`,
	} {
		if _, err := extension.ParseContainerConfig([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if _, err := extension.ParseContainerConfig(containerConfig(testImage, `,"limits":{"cpus":4,"memory":"8g","pids":2048}`).Data); err != nil {
		t.Errorf("good config: %v", err)
	}
}

// launched launches a container, starts it as the wrapper would, and
// returns the pin a recovering segment carries.
func launched(t *testing.T, h *containerHost, f *fakeContainers) *extension.ContainerPin {
	t.Helper()
	if _, err := h.bind(f, containerConfig(testImage, ""), nil).Launch(context.Background(), h.needs(), h.launch()); err != nil {
		t.Fatal(err)
	}
	f.start("c1", h.now.Add(time.Second))
	pin := h.pins[0]
	return &pin
}

func TestContainerRecover(t *testing.T) {
	ctx := context.Background()
	cfg := containerConfig(testImage, "")
	t.Run("wrapper_alive_adopts_the_pinned_container", func(t *testing.T) {
		h, f := newContainerHost(t), newFakeContainers()
		pin := launched(t, h, f)
		if exit, err := h.bind(f, cfg, pin).Adopt(ctx, true); err != nil || exit.Exited {
			t.Fatalf("adopt: %+v %v", exit, err)
		}
	})
	t.Run("wrapper_alive_refuses_an_unproven_container", func(t *testing.T) {
		for name, change := range map[string]func(*extension.ContainerState){
			"labels": func(c *extension.ContainerState) {
				c.Ref.Labels = map[string]string{extension.ContainerLabelExecution: "x"}
			},
			"image":      func(c *extension.ContainerState) { c.Image = "ghcr.io/acme/other@sha256:" + strings.Repeat("f", 64) },
			"started_at": func(c *extension.ContainerState) { c.StartedAt = c.StartedAt.Add(-time.Hour) },
		} {
			h, f := newContainerHost(t), newFakeContainers()
			pin := launched(t, h, f)
			change(f.containers["c1"])
			if _, err := h.bind(f, cfg, pin).Adopt(ctx, true); !errors.Is(err, extension.ErrNotAdoptable) {
				t.Errorf("%s: %v, want ErrNotAdoptable", name, err)
			}
		}
	})
	t.Run("wrapper_dead_container_exited_returns_its_code", func(t *testing.T) {
		h, f := newContainerHost(t), newFakeContainers()
		pin := launched(t, h, f)
		f.containers["c1"].Running, f.containers["c1"].ExitCode = false, 3
		exit, err := h.bind(f, cfg, pin).Adopt(ctx, false)
		if err != nil || !exit.Exited || exit.ExitCode != 3 || exit.Interrupted {
			t.Fatalf("adopt: %+v %v, want exit code 3", exit, err)
		}
	})
	t.Run("wrapper_dead_container_running_is_stopped_before_a_retry", func(t *testing.T) {
		h, f := newContainerHost(t), newFakeContainers()
		pin := launched(t, h, f)
		if _, err := h.bind(f, cfg, pin).Adopt(ctx, false); !errors.Is(err, extension.ErrNotAdoptable) {
			t.Fatalf("adopt: %v, want ErrNotAdoptable", err)
		}
		if err := extension.StopContainer(ctx, f, pin, h.records, time.Second); err != nil {
			t.Fatalf("stop: %v", err)
		}
		if !contains(f.stopped, "c1") || f.containers["c1"].Running {
			t.Error("the container still runs")
		}
		if !extension.ContainerSignalled(h.records, "c1") {
			t.Error("the host's stop was not recorded")
		}
	})
	t.Run("stop_confirms_only_once_not_running", func(t *testing.T) {
		h, f := newContainerHost(t), newFakeContainers()
		pin := launched(t, h, f)
		f.stopFails = true
		if err := extension.StopContainer(ctx, f, pin, h.records, time.Second); !errors.Is(err, extension.ErrContainerUnconfirmed) {
			t.Fatalf("stop: %v, want ErrContainerUnconfirmed", err)
		}
	})
	t.Run("container_gone_on_the_same_daemon_lets_replacement_go_ahead", func(t *testing.T) {
		h, f := newContainerHost(t), newFakeContainers()
		pin := launched(t, h, f)
		delete(f.containers, "c1")
		if _, err := h.bind(f, cfg, pin).Adopt(ctx, false); !errors.Is(err, extension.ErrNotAdoptable) {
			t.Fatalf("adopt: %v, want ErrNotAdoptable", err)
		}
		if err := extension.StopContainer(ctx, f, pin, h.records, time.Second); err != nil {
			t.Fatalf("stop: %v, want gone confirmed as not running", err)
		}
	})
	t.Run("daemon_unreachable_blocks_replacement", func(t *testing.T) {
		h, f := newContainerHost(t), newFakeContainers()
		pin := launched(t, h, f)
		f.inspectErr = errors.New("cannot connect")
		if _, err := h.bind(f, cfg, pin).Adopt(ctx, true); !errors.Is(err, extension.ErrNotAdoptable) {
			t.Fatalf("adopt: %v, want ErrNotAdoptable", err)
		}
		if err := extension.StopContainer(ctx, f, pin, h.records, time.Second); !errors.Is(err, extension.ErrContainerUnconfirmed) {
			t.Fatalf("stop: %v, want ErrContainerUnconfirmed", err)
		}
	})
	t.Run("different_daemon_blocks_replacement", func(t *testing.T) {
		h, f := newContainerHost(t), newFakeContainers()
		pin := launched(t, h, f)
		f.daemon = "daemon-2"
		delete(f.containers, "c1")
		if _, err := h.bind(f, cfg, pin).Adopt(ctx, false); !errors.Is(err, extension.ErrNotAdoptable) {
			t.Fatalf("adopt: %v, want ErrNotAdoptable", err)
		}
		if err := extension.StopContainer(ctx, f, pin, h.records, time.Second); !errors.Is(err, extension.ErrContainerUnconfirmed) {
			t.Fatalf("stop: %v, want ErrContainerUnconfirmed: gone from another daemon proves nothing", err)
		}
	})
	t.Run("no_pin_no_adoption", func(t *testing.T) {
		h, f := newContainerHost(t), newFakeContainers()
		if _, err := h.bind(f, cfg, nil).Adopt(ctx, true); !errors.Is(err, extension.ErrNotAdoptable) {
			t.Fatalf("adopt: %v, want ErrNotAdoptable", err)
		}
	})
}

func TestContainerUnrequestedTerm(t *testing.T) {
	ctx := context.Background()
	h, f := newContainerHost(t), newFakeContainers()
	b := h.bind(f, containerConfig(testImage, ""), nil)
	if _, err := b.Launch(ctx, h.needs(), h.launch()); err != nil {
		t.Fatal(err)
	}
	done := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted, Summary: "exit status 0", Data: domain.Payload{Schema: "x.v1", Data: json.RawMessage(`{}`)}}}
	if got := b.Settle(done); got.Outcome.Kind != domain.OutcomeCompleted {
		t.Fatalf("no TERM: %+v", got.Outcome)
	}
	// The entry stub saw a TERM nobody in Open Conduit sent.
	if err := os.WriteFile(filepath.Join(h.run, extension.ContainerTermMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := b.Settle(done)
	if got.Outcome.Kind != domain.OutcomeFailed || !strings.Contains(got.Outcome.Summary, "interrupted") || got.Outcome.Data.Schema != "x.v1" {
		t.Fatalf("unrequested TERM: %+v, want interrupted with the executor's data kept", got.Outcome)
	}
	// The same exit, recovered without its wrapper, is interrupted too.
	f.start("c1", h.now.Add(time.Second))
	f.containers["c1"].Running = false
	exit, err := h.bind(f, containerConfig(testImage, ""), &h.pins[0]).Adopt(ctx, false)
	if err != nil || !exit.Interrupted {
		t.Fatalf("recovered exit %+v %v, want interrupted", exit, err)
	}
	// A TERM the host sent is the host's.
	if err := extension.RecordContainerSignal(h.records, "c1", "term"); err != nil {
		t.Fatal(err)
	}
	if got := b.Settle(done); got.Outcome.Kind != domain.OutcomeCompleted {
		t.Fatalf("host TERM: %+v, want the executor's result", got.Outcome)
	}
}

func TestReconcileContainers(t *testing.T) {
	f := newFakeContainers()
	add := func(id, host, exec string) {
		f.containers[id] = &extension.ContainerState{Ref: extension.ContainerRef{ID: id, Labels: map[string]string{extension.ContainerLabelHost: host, extension.ContainerLabelExecution: exec}}}
	}
	add("live", "host-a", "x1")
	add("terminal", "host-a", "x2")
	add("gone", "host-a", "x3")
	add("other-host", "host-b", "x2")
	err := extension.ReconcileContainers(context.Background(), f, "host-a", func(ids []domain.ID) (map[domain.ID]bool, error) {
		return map[domain.ID]bool{"x1": true, "x2": false}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.removed) != 2 || !contains(f.removed, "terminal") || !contains(f.removed, "gone") {
		t.Errorf("removed %v, want the terminal and gone executions' containers only", f.removed)
	}
	f.listErr = errors.New("unreachable")
	if err := extension.ReconcileContainers(context.Background(), f, "host-a", nil); err == nil {
		t.Error("a failed listing reconciled")
	}
}

func TestRemoveAndMountsFailClosed(t *testing.T) {
	ctx := context.Background()
	owner := extension.ResourceOwner{ExecutionID: "x"}
	f := newFakeContainers()
	f.containers["run"] = &extension.ContainerState{Ref: extension.ContainerRef{ID: "run", Labels: map[string]string{extension.ContainerLabelExecution: "x"}}, Running: true, Mounts: []string{"/ws"}}
	if m, err := extension.ContainerMounts(ctx, f, owner); err != nil || len(m) != 1 || m[0] != "/ws" {
		t.Errorf("mounts %v %v", m, err)
	}
	if err := extension.RemoveContainers(ctx, f, owner); !errors.Is(err, extension.ErrContainerRunning) {
		t.Errorf("remove a running container: %v", err)
	}
	f.containers["run"].Running = false
	if err := extension.RemoveContainers(ctx, f, owner); err != nil || len(f.containers) != 0 {
		t.Errorf("remove: %v, left %v", err, f.containers)
	}
	f.listErr = errors.New("unreachable")
	if _, err := extension.ContainerMounts(ctx, f, owner); err == nil {
		t.Error("mounts from a failed listing")
	}
	if err := extension.RemoveContainers(ctx, f, owner); err == nil {
		t.Error("remove after a failed listing")
	}
}

func TestGuardLaunchIsShared(t *testing.T) {
	h := newSandboxHost(t)
	guard := extension.LaunchGuard{Protected: []string{h.state}, Shared: []string{filepath.Dir(h.toolbelt)}, RunRoot: h.runRoot, Toolbelt: h.toolbelt,
		Records: h.records, Dir: h.ws, RunDir: h.run, Needs: extension.SandboxNeeds{Write: []string{h.ws}, Sockets: []string{h.toolbelt}}}
	g, err := extension.GuardLaunch(guard)
	if err != nil || g.Workspace != h.ws || g.Run != h.run || !contains(g.Protected, h.state) {
		t.Fatalf("guard %+v %v", g, err)
	}
	link := filepath.Join(h.ws, "token")
	if err := os.Link(filepath.Join(h.state, "private", "operator.token"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := extension.GuardLaunch(guard); !errors.Is(err, extension.ErrSandboxDenied) {
		t.Fatalf("hard link: %v, want ErrSandboxDenied", err)
	}
	// The same refusal reaches both brokers.
	if _, err := extension.WrapLaunch(context.Background(), h.bind(&fakeSandbox{}, ""), guard.Needs, h.launch()); !errors.Is(err, extension.ErrSandboxDenied) {
		t.Errorf("sandbox: %v", err)
	}
	ch := &containerHost{sandboxHost: h, home: h.ws, now: time.Now()}
	if _, err := ch.bind(newFakeContainers(), containerConfig(testImage, ""), nil).Launch(context.Background(), guard.Needs, h.launch()); !errors.Is(err, extension.ErrSandboxDenied) {
		t.Errorf("container: %v", err)
	}
	_ = os.Remove(link)
	guard.Needs.Read = []string{filepath.Join(h.state, "private")}
	if _, err := extension.GuardLaunch(guard); !errors.Is(err, extension.ErrSandboxDenied) {
		t.Fatalf("protected read: %v, want ErrSandboxDenied", err)
	}
}

func TestHostAllowed(t *testing.T) {
	allow := []string{"api.anthropic.com", "*.github.com", "proxy.local:8080"}
	for host, want := range map[string]bool{
		"api.anthropic.com": true, "API.anthropic.com": true, "codeload.github.com": true, "github.com": false,
		"proxy.local:8080": true, "proxy.local:9090": false, "proxy.local": false, "evil.example": false,
	} {
		if got := extension.HostAllowed(host, allow); got != want {
			t.Errorf("%s: %v, want %v", host, got, want)
		}
	}
}
