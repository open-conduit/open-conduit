package tracker

import "testing"

// stubService is a distinct Service value; no method is called.
type stubService struct {
	Service
	id int
}

// Stopping a tracker extension withdraws its entries from the reopen
// registry, but never another daemon's entry registered since under the
// same namespace.
func TestStopWithdrawsOwnReopens(t *testing.T) {
	register := func(ns string, svc Service) *Extension {
		x := NewExtension(Kind{Type: "linear"})
		x.sources[ns] = &source{service: svc}
		reopens.Lock()
		reopens.m[ns] = svc
		reopens.Unlock()
		return x
	}
	first := register("eng-linear", stubService{id: 1})
	if !Registered("eng-linear") {
		t.Fatal("a registered entry is not found")
	}
	first.Stop()
	if Registered("eng-linear") {
		t.Fatal("a stopped extension's entry is still registered")
	}
	old := register("ops-linear", stubService{id: 2})
	newer := register("ops-linear", stubService{id: 3})
	old.Stop()
	if !Registered("ops-linear") {
		t.Fatal("stopping an old extension withdrew a newer one's entry")
	}
	newer.Stop()
	if Registered("ops-linear") {
		t.Fatal("the newer entry outlived its extension")
	}
}
