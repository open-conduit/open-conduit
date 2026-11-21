package bitbucket

import (
	"strconv"
	"testing"
	"time"
)

// A pass looks back only the closed horizon, so an interval beyond it would
// lose closes for good.
func TestReconcileSecondsMustFitTheHorizon(t *testing.T) {
	for seconds, ok := range map[int]bool{3600: true, 3601: false, -1: false} {
		s, err := ParseExtension([]byte(`{"namespace":"bitbucket/acme","scope":"eng","repo":"acme/widgets","closed_horizon_hours":1,"reconcile_seconds":`+strconv.Itoa(seconds)+`}`), nil)
		if (err == nil) != ok {
			t.Fatalf("%d: %v", seconds, err)
		}
		if ok && s.Reconcile != time.Duration(seconds)*time.Second {
			t.Fatalf("%d: interval %s", seconds, s.Reconcile)
		}
	}
}
