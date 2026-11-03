package management

import (
	"testing"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// A genuine token whose execution has declared a wait or outcome is refused
// with its own code, so the caller can tell it already parked the run apart
// from a bad, forged or superseded token, which keeps the bare refusal.
func TestMapErrorTellsReleasedAuthorityFromRejected(t *testing.T) {
	released := mapError(extension.ErrAuthorityReleased)
	if released.status != 403 || released.code != "authority_released" {
		t.Fatalf("released authority mapped to %d %s", released.status, released.code)
	}
	for _, err := range []error{extension.ErrUnauthorized, storage.ErrLeaseLost} {
		if got := mapError(err); got.status != 403 || got.code != "forbidden" {
			t.Fatalf("%v mapped to %d %s", err, got.status, got.code)
		}
	}
}
