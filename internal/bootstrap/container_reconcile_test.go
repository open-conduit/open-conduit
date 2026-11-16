package bootstrap

import (
	"context"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// listedProvider is a container provider holding a fixed listing, which
// records each Remove.
type listedProvider struct {
	extension.ContainerProvider
	listed  []extension.ContainerState
	removed []string
}

func (p *listedProvider) List(context.Context, extension.ResourceOwner) ([]extension.ContainerState, error) {
	return p.listed, nil
}

func (p *listedProvider) Remove(_ context.Context, ref extension.ContainerRef) error {
	p.removed = append(p.removed, ref.ID)
	return nil
}

// TestParkedExecutionsReleaseTheirContainers (#669): reconcile removes the
// stopped container of a parked execution, as it does a finished one's, so
// a waiting conversation holds nothing a provider made for its last
// segment (Docker's networks, of which an engine has few). It keeps a
// parked execution's attach, which an operator may be launching with
// nothing of it running yet; a finished execution's stopped attach goes.
// The negative control: a pending or running execution's containers stay,
// the stopped one too, since recovery may adopt it.
func TestParkedExecutionsReleaseTheirContainers(t *testing.T) {
	states := map[domain.ID]domain.ExecutionState{
		"x-pending": domain.ExecutionPending, "x-running": domain.ExecutionRunning, "x-parked": domain.ExecutionParked,
		"x-waiting": domain.ExecutionWaiting, "x-completed": domain.ExecutionCompleted, "x-cancelled": domain.ExecutionCancelled,
	}
	p := &listedProvider{}
	for id := range states {
		for _, segment := range []string{"1", extension.ContainerSegmentAttach} {
			p.listed = append(p.listed, extension.ContainerState{Ref: extension.ContainerRef{ID: string(id) + "/" + segment,
				Labels: map[string]string{extension.ContainerLabelHost: "host", extension.ContainerLabelExecution: string(id), extension.ContainerLabelSegment: segment}}})
		}
	}
	_, err := extension.ReconcileContainersKeeping(context.Background(), p, "host", func(ids []domain.ID) (map[domain.ID]extension.ContainerKeep, error) {
		keep := map[domain.ID]extension.ContainerKeep{}
		for _, id := range ids {
			keep[id] = containersKept(states[id])
		}
		return keep, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	removed := map[string]bool{}
	for _, id := range p.removed {
		removed[id] = true
	}
	for id, state := range states {
		live := state == domain.ExecutionPending || state == domain.ExecutionRunning
		if got := removed[string(id)+"/1"]; got == live {
			t.Errorf("%s execution's segment container: removed %v, want %v", state, got, !live)
		}
		want := !live && state != domain.ExecutionParked
		if got := removed[string(id)+"/"+extension.ContainerSegmentAttach]; got != want {
			t.Errorf("%s execution's stopped attach: removed %v, want %v", state, got, want)
		}
	}
}
