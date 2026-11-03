package linear

import (
	"encoding/json"
	"time"

	"github.com/open-conduit/open-conduit/extensions/tracker"
)

// New returns the Linear extension: the tracker kit's in-process extension
// for Linear entries.
func New() *tracker.Extension { return tracker.NewExtension(Kind()) }

// Kind is the tracker kit's description of Linear: its type, validators and client.
func Kind() tracker.Kind {
	return tracker.Kind{Type: Type, ValidateConnection: ValidateConnection, ValidateExtension: ValidateExtension, Open: open, HookPermission: nil, Setup: Setup{}}
}

// open builds an entry's settings and Linear client.
func open(conn, entry json.RawMessage, secretNames []string, secrets tracker.SecretFunc, now func() time.Time, dryRun bool) (tracker.Settings, tracker.Service, error) {
	cfg, err := ParseConnection(conn)
	if err != nil {
		return tracker.Settings{}, nil, tracker.CodeError("linear_connection_invalid")
	}
	st, err := ParseExtension(entry, secretNames)
	if err != nil {
		return tracker.Settings{}, nil, err
	}
	if dryRun {
		st.Projection.DryRun = true
	}
	client, err := NewClient(cfg, st, secrets, ClientOptions{Now: now, DryRun: st.Projection.DryRun})
	if err != nil {
		return tracker.Settings{}, nil, err
	}
	return st.Settings, client, nil
}
