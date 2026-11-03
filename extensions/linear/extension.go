package linear

import (
	"encoding/json"
	"time"

	"github.com/open-conduit/open-conduit/extensions/tracker"
	"github.com/open-conduit/open-conduit/protocol"
)

// New returns the Linear extension: the tracker kit's in-process extension
// for Linear entries.
func New() *tracker.Extension { return tracker.NewExtension(Kind()) }

// Kind is the tracker kit's description of Linear: its type, validators and client.
func Kind() tracker.Kind {
	return tracker.Kind{Type: Type, ValidateConnection: ValidateConnection, ValidateExtension: ValidateExtension, Open: open, HookPermission: nil, Setup: Setup{},
		ConnectionFields: connectionFields(), EntryFields: entryFields()}
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

// connectionFields describe the linear connection's config and secret_refs
// for the operator's catalogue.
func connectionFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "api_url", Default: DefaultAPIURL},
		{Name: "agent_account", Help: "a user whose comments always count as an agent's"},
		{Name: SecretToken, Help: "a personal API key, or an OAuth token (lin_oauth_…)", Required: true, Secret: true},
	}
}

// entryFields are the entry config fields linear adds to the tracker kit's.
func entryFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "teams", Help: "team keys (ENG)", Required: true},
		{Name: "states", Help: "state types or names issues are claimed from; others wait", Default: "unstarted, started"},
	}
}
