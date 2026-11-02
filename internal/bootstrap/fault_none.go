//go:build !faultinject

package bootstrap

import (
	"log/slog"

	"github.com/open-conduit/open-conduit/storage"
)

// faultStore is the identity in production builds. The faultinject build tag
// replaces it with the kill-drill crash injector (see fault_inject.go).
func faultStore(s storage.Store, _ *slog.Logger) storage.Store { return s }

// passed marks a completed host loop pass. It does nothing in production
// builds; the faultinject build publishes the count as the integration
// tests' loop barrier (see fault_inject.go).
func (d *daemon) passed() {}
