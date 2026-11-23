//go:build !faultinject

package bootstrap

import (
	"log/slog"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

// faultStore is the identity in production builds. The faultinject build tag
// replaces it with the kill-drill crash injector (see fault_inject.go).
func faultStore(s storage.Store, _ *slog.Logger) storage.Store { return s }

// passed marks a completed host loop pass. It does nothing in production
// builds; the faultinject build publishes the count as the integration
// tests' loop barrier (see fault_inject.go).
func (d *daemon) passed() {}

// startAll reports whether every catalogued package starts, whether or
// not anything uses it: never in production builds. The faultinject build
// reads it from the environment, as the integration tests' negative
// control for starting only the packages the config uses (fault_inject.go).
func startAll() bool { return false }

// faultPace leaves the instance lease's pace alone in production builds. The
// faultinject build can shorten it (OPEN_CONDUIT_INSTANCE_TTL).
func faultPace(*Options) {}

// daemonClock is the daemon's clock. The faultinject build can run it ahead
// (OPEN_CONDUIT_CLOCK_SKEW), so a test can age history without waiting.
func daemonClock() func() time.Time { return time.Now }
