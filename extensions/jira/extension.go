package jira

import (
	"encoding/json"
	"time"

	"github.com/open-conduit/open-conduit/extensions/tracker"
	"github.com/open-conduit/open-conduit/protocol"
)

// New returns the Jira extension: the tracker kit's in-process extension
// for Jira entries.
func New() *tracker.Extension { return tracker.NewExtension(Kind()) }

// Kind is the tracker kit's description of Jira: its type, validators and client.
func Kind() tracker.Kind {
	return tracker.Kind{Type: Type, ValidateConnection: ValidateConnection, ValidateExtension: ValidateExtension, Open: open, HookPermission: ErrHookPermission, Setup: Setup{},
		ConnectionFields: connectionFields(), EntryFields: entryFields()}
}

// open builds an entry's settings and Jira client.
func open(conn, entry json.RawMessage, secretNames []string, secrets tracker.SecretFunc, now func() time.Time, dryRun bool) (tracker.Settings, tracker.Service, error) {
	cfg, err := ParseConnection(conn)
	if err != nil {
		return tracker.Settings{}, nil, tracker.CodeError("jira_connection_invalid")
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

// connectionFields describe the jira connection's config and secret_refs
// for the operator's catalogue.
func connectionFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "base_url", Help: "https://acme.atlassian.net", Required: true},
		{Name: "auth", Help: "basic: email + API token (Cloud); bearer: a personal access token", Default: "basic", Options: []string{"basic", "bearer"}},
		{Name: "email", Help: "for basic auth"},
		{Name: "agent_account", Help: "a user whose comments always count as an agent's"},
		{Name: SecretToken, Help: "API token or personal access token", Required: true, Secret: true},
	}
}

// entryFields are the entry config fields jira adds to the tracker kit's.
func entryFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "projects", Help: "project keys (PROJ)", Required: true},
		{Name: "jql", Help: "ANDed with the project and open filter"},
		{Name: "fields", Help: "extra field ids routes match on (customfield_10010)"},
	}
}
