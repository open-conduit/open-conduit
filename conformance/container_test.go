package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// memContainers is an in-memory container provider that keeps the
// contract, and a base for the negative controls.
type memContainers struct {
	mu         sync.Mutex
	containers map[string]extension.ContainerState
	n          int
}

func (m *memContainers) Plan(context.Context, extension.ResourceOwner, extension.ContainerPolicy, extension.SandboxNeeds, extension.SandboxLaunch) (extension.ContainerPlan, error) {
	return extension.ContainerPlan{Image: "mem@sha256:" + strings.Repeat("a", 64), Data: domain.Payload{Schema: "mem.plan", Data: json.RawMessage(`{}`)}}, nil
}

func (m *memContainers) Create(_ context.Context, p extension.ContainerPlan, labels map[string]string) (extension.ContainerCreated, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	ref := extension.ContainerRef{ID: "mem/" + string(rune('a'+m.n)), Labels: labels}
	m.containers[ref.ID] = extension.ContainerState{Ref: ref, Daemon: "mem", Image: p.Image}
	return extension.ContainerCreated{Ref: ref, Daemon: "mem", Argv: []string{"start", ref.ID}}, nil
}

func (m *memContainers) run(c extension.ContainerCreated, code int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.containers[c.Ref.ID]
	st.StartedAt, st.ExitCode = time.Now(), code
	m.containers[c.Ref.ID] = st
	return code
}

func (m *memContainers) Signal(context.Context, extension.ContainerRef, string) error { return nil }
func (m *memContainers) Stop(context.Context, extension.ContainerRef, time.Duration) error {
	return nil
}

func (m *memContainers) Inspect(_ context.Context, ref extension.ContainerRef) (extension.ContainerState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.containers[ref.ID]
	if !ok {
		return extension.ContainerState{Ref: ref, Daemon: "mem", Gone: true}, nil
	}
	return st, nil
}

func (m *memContainers) List(_ context.Context, o extension.ResourceOwner) ([]extension.ContainerState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []extension.ContainerState
	for _, st := range m.containers {
		if o.ExecutionID == "" || st.Ref.Labels[extension.ContainerLabelExecution] == string(o.ExecutionID) {
			out = append(out, st)
		}
	}
	return out, nil
}

func (m *memContainers) Remove(_ context.Context, ref extension.ContainerRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.containers, ref.ID)
	return nil
}

// unfiltered lists every container whatever the owner.
type unfiltered struct{ *memContainers }

func (u unfiltered) List(ctx context.Context, _ extension.ResourceOwner) ([]extension.ContainerState, error) {
	return u.memContainers.List(ctx, extension.ResourceOwner{})
}

// goneIsError reports a removed container as an error, not Gone.
type goneIsError struct{ *memContainers }

func (g goneIsError) Inspect(ctx context.Context, ref extension.ContainerRef) (extension.ContainerState, error) {
	st, err := g.memContainers.Inspect(ctx, ref)
	if st.Gone {
		return extension.ContainerState{}, errors.New("no such container")
	}
	return st, err
}

// startedAtCreate reports a container started as it is created.
type startedAtCreate struct{ *memContainers }

func (s startedAtCreate) Inspect(ctx context.Context, ref extension.ContainerRef) (extension.ContainerState, error) {
	st, err := s.memContainers.Inspect(ctx, ref)
	if !st.Gone && st.StartedAt.IsZero() {
		st.Running, st.StartedAt = true, time.Now()
	}
	return st, err
}

func TestCheckContainerProvider(t *testing.T) {
	fixture := func(p extension.ContainerProvider, m *memContainers) ContainerFixture {
		return ContainerFixture{Provider: p, Host: "h", Code: 3, Start: func(c extension.ContainerCreated) int { return m.run(c, 3) }}
	}
	m := &memContainers{containers: map[string]extension.ContainerState{}}
	if err := CheckContainerProvider(fixture(m, m)); err != nil {
		t.Fatalf("a provider that keeps the contract fails: %v", err)
	}
	// Negative controls: each breaks one rule and must fail.
	for name, broken := range map[string]func(*memContainers) extension.ContainerProvider{
		"listing ignores the owner":      func(m *memContainers) extension.ContainerProvider { return unfiltered{m} },
		"gone is an error":               func(m *memContainers) extension.ContainerProvider { return goneIsError{m} },
		"created is reported as started": func(m *memContainers) extension.ContainerProvider { return startedAtCreate{m} },
	} {
		m := &memContainers{containers: map[string]extension.ContainerState{}}
		if err := CheckContainerProvider(fixture(broken(m), m)); err == nil {
			t.Errorf("%s: the suite passed a provider that breaks the contract", name)
		}
	}
}

// memCollector is memContainers whose runs write somewhere other than the
// host: what an orphaned run wrote waits in remote until Collect brings it
// back to file, once. A base for the collect controls.
type memCollector struct {
	*memContainers
	file, content string
	remote        map[string]string
	collected     map[string]bool
}

func newMemCollector(file string) *memCollector {
	return &memCollector{memContainers: &memContainers{containers: map[string]extension.ContainerState{}}, file: file, content: "the run's work",
		remote: map[string]string{}, collected: map[string]bool{}}
}

// orphan starts the run and loses its wrapper: the run writes on the
// remote and exits, and nothing brings its work back.
func (m *memCollector) orphan(c extension.ContainerCreated) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.containers[c.Ref.ID]
	st.StartedAt, st.ExitCode = time.Now(), 5
	m.containers[c.Ref.ID] = st
	m.remote[c.Ref.ID] = m.content
	return nil
}

func (m *memCollector) Collect(_ context.Context, ref extension.ContainerRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.containers[ref.ID]; ok && st.Running {
		return errors.New("still running")
	}
	work, ok := m.remote[ref.ID]
	if !ok || m.collected[ref.ID] {
		return nil
	}
	m.collected[ref.ID] = true
	return os.WriteFile(m.file, []byte(work), 0o644)
}

// noopCollect declares collect and brings nothing back.
type noopCollect struct{ *memCollector }

func (noopCollect) Collect(context.Context, extension.ContainerRef) error { return nil }

// collectsEveryTime brings the run's work back on every collect, over
// whatever the host did since.
type collectsEveryTime struct{ *memCollector }

func (c collectsEveryTime) Collect(ctx context.Context, ref extension.ContainerRef) error {
	c.mu.Lock()
	delete(c.collected, ref.ID)
	c.mu.Unlock()
	return c.memCollector.Collect(ctx, ref)
}

func TestCheckContainerCollector(t *testing.T) {
	fixture := func(p interface {
		extension.ContainerProvider
		extension.ContainerCollector
	}, m *memCollector) CollectorFixture {
		return CollectorFixture{Provider: p, Host: "h", Code: 5, File: m.file, Content: m.content, Orphan: m.orphan,
			Policy: extension.ContainerPolicy{Paths: extension.SandboxPaths{Run: t.TempDir(), Records: t.TempDir()}}}
	}
	m := newMemCollector(filepath.Join(t.TempDir(), "out.txt"))
	if err := CheckContainerCollector(fixture(m, m)); err != nil {
		t.Fatalf("a provider that keeps the contract fails: %v", err)
	}
	// Negative controls: each breaks one rule and must fail.
	for name, broken := range map[string]func(*memCollector) interface {
		extension.ContainerProvider
		extension.ContainerCollector
	}{
		"collect is a no-op": func(m *memCollector) interface {
			extension.ContainerProvider
			extension.ContainerCollector
		} {
			return noopCollect{m}
		},
		"collecting again undoes the host's change": func(m *memCollector) interface {
			extension.ContainerProvider
			extension.ContainerCollector
		} {
			return collectsEveryTime{m}
		},
	} {
		m := newMemCollector(filepath.Join(t.TempDir(), "out.txt"))
		if err := CheckContainerCollector(fixture(broken(m), m)); err == nil {
			t.Errorf("%s: the suite passed a provider that breaks the contract", name)
		}
	}
}

// memPlacer places launches on nodes and runs site commands on this
// machine, keeping the placement contract; a base for its controls.
type memPlacer struct {
	*memContainers
	placed    map[string]bool
	plannedAt string
}

func newMemPlacer() *memPlacer {
	return &memPlacer{memContainers: &memContainers{containers: map[string]extension.ContainerState{}}, placed: map[string]bool{}}
}

func (m *memPlacer) Place(_ context.Context, _ extension.ResourceOwner, _ extension.ContainerPolicy, prefer string) (extension.Placement, error) {
	id := prefer
	if !m.placed[id] {
		id = "node-" + string(rune('a'+len(m.placed)))
	}
	m.placed[id] = true
	return extension.Placement{ID: id, Daemon: "mem-" + id}, nil
}

func (m *memPlacer) SiteRun(ctx context.Context, placement string, cmd extension.SiteCommand) (extension.SiteResult, error) {
	if !m.placed[placement] {
		return extension.SiteResult{}, extension.ErrSiteUnavailable
	}
	c := exec.CommandContext(ctx, cmd.Argv[0], cmd.Argv[1:]...)
	c.Dir, c.Env, c.Stdin = cmd.Dir, cmd.Env, bytes.NewReader(cmd.Stdin)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return extension.SiteResult{}, err
	}
	return extension.SiteResult{ExitCode: c.ProcessState.ExitCode(), Stdout: out.Bytes(), Stderr: errb.Bytes()}, nil
}

func (m *memPlacer) Plan(ctx context.Context, o extension.ResourceOwner, p extension.ContainerPolicy, n extension.SandboxNeeds, l extension.SandboxLaunch) (extension.ContainerPlan, error) {
	m.plannedAt = p.Placement
	return m.memContainers.Plan(ctx, o, p, n, l)
}

func (m *memPlacer) Create(ctx context.Context, p extension.ContainerPlan, labels map[string]string) (extension.ContainerCreated, error) {
	c, err := m.memContainers.Create(ctx, p, labels)
	c.Daemon = "mem-" + m.plannedAt
	return c, err
}

type placer interface {
	extension.ContainerProvider
	extension.ContainerPlacer
}

// failureIsError reports a command that ran and failed as an error.
type failureIsError struct{ *memPlacer }

func (f failureIsError) SiteRun(ctx context.Context, at string, cmd extension.SiteCommand) (extension.SiteResult, error) {
	r, err := f.memPlacer.SiteRun(ctx, at, cmd)
	if err == nil && r.ExitCode != 0 {
		return r, errors.New("command failed")
	}
	return r, err
}

// ignoresPrefer places each launch on a new node.
type ignoresPrefer struct{ *memPlacer }

func (i ignoresPrefer) Place(ctx context.Context, o extension.ResourceOwner, p extension.ContainerPolicy, _ string) (extension.Placement, error) {
	return i.memPlacer.Place(ctx, o, p, "")
}

// createsElsewhere creates every container on one daemon, wherever placed.
type createsElsewhere struct{ *memPlacer }

func (c createsElsewhere) Create(ctx context.Context, p extension.ContainerPlan, labels map[string]string) (extension.ContainerCreated, error) {
	out, err := c.memPlacer.Create(ctx, p, labels)
	out.Daemon = "mem"
	return out, err
}

// runsAnywhere runs a command at a placement it never gave.
type runsAnywhere struct{ *memPlacer }

func (r runsAnywhere) SiteRun(ctx context.Context, _ string, cmd extension.SiteCommand) (extension.SiteResult, error) {
	for at := range r.placed {
		return r.memPlacer.SiteRun(ctx, at, cmd)
	}
	return extension.SiteResult{}, extension.ErrSiteUnavailable
}

// ignoresDir runs every command in the site's home.
type ignoresDir struct{ *memPlacer }

func (i ignoresDir) SiteRun(ctx context.Context, at string, cmd extension.SiteCommand) (extension.SiteResult, error) {
	cmd.Dir = ""
	return i.memPlacer.SiteRun(ctx, at, cmd)
}

func TestCheckContainerPlacer(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	fixture := func(p placer) PlacerFixture { return PlacerFixture{Provider: p, Dir: dir} }
	if err := CheckContainerPlacer(fixture(newMemPlacer())); err != nil {
		t.Fatalf("a placer that keeps the contract fails: %v", err)
	}
	for name, broken := range map[string]func(*memPlacer) placer{
		"a failed command is an error":       func(m *memPlacer) placer { return failureIsError{m} },
		"prefer is ignored":                  func(m *memPlacer) placer { return ignoresPrefer{m} },
		"created elsewhere than placed":      func(m *memPlacer) placer { return createsElsewhere{m} },
		"runs at a placement it never gave":  func(m *memPlacer) placer { return runsAnywhere{m} },
		"the command's directory is ignored": func(m *memPlacer) placer { return ignoresDir{m} },
	} {
		if err := CheckContainerPlacer(fixture(broken(newMemPlacer()))); err == nil {
			t.Errorf("%s: the suite passed a placer that breaks the contract", name)
		}
	}
}
