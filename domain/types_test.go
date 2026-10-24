package domain_test

import (
	"github.com/alsiola/open-conduit/domain"
	"testing"
)

func TestWorkLifecycle(t *testing.T) {
	states := []domain.WorkState{domain.WorkReady, domain.WorkRunning, domain.WorkWaiting, domain.WorkCompleted, domain.WorkFailed, domain.WorkCancelled, "unknown"}
	allowed := map[domain.WorkState][]domain.WorkState{
		domain.WorkReady:   {domain.WorkRunning, domain.WorkFailed, domain.WorkCancelled},
		domain.WorkRunning: {domain.WorkReady, domain.WorkWaiting, domain.WorkCompleted, domain.WorkFailed, domain.WorkCancelled},
		domain.WorkWaiting: {domain.WorkReady, domain.WorkRunning, domain.WorkFailed, domain.WorkCancelled},
	}
	for _, from := range states {
		for _, to := range states {
			want := false
			for _, target := range allowed[from] {
				if target == to {
					want = true
				}
			}
			if got := domain.WorkTransition(from, to); got != want {
				t.Errorf("%s -> %s = %t, want %t", from, to, got, want)
			}
		}
	}
}
func TestExecutionLifecycle(t *testing.T) {
	states := []domain.ExecutionState{domain.ExecutionPending, domain.ExecutionRunning, domain.ExecutionParked, domain.ExecutionCompleted, domain.ExecutionFailed, domain.ExecutionWaiting, domain.ExecutionHandoff, domain.ExecutionCancelled, "unknown"}
	allowed := map[domain.ExecutionState][]domain.ExecutionState{
		domain.ExecutionPending: {domain.ExecutionRunning, domain.ExecutionCancelled},
		domain.ExecutionRunning: {domain.ExecutionParked, domain.ExecutionCompleted, domain.ExecutionFailed, domain.ExecutionWaiting, domain.ExecutionHandoff, domain.ExecutionCancelled},
		domain.ExecutionParked:  {domain.ExecutionRunning, domain.ExecutionFailed, domain.ExecutionCancelled},
	}
	for _, from := range states {
		for _, to := range states {
			want := false
			for _, target := range allowed[from] {
				if target == to {
					want = true
				}
			}
			if got := domain.ExecutionTransition(from, to); got != want {
				t.Errorf("%s -> %s = %t, want %t", from, to, got, want)
			}
		}
	}
}
