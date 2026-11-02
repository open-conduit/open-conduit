package conformance

import (
	"context"
	"encoding/json"
	"errors"
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
