package posthog

import "github.com/open-conduit/open-conduit/protocol"

// connectionFields describe the posthog connection's config and
// secret_refs for the operator's catalogue (manifest fields).
func connectionFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "host", Help: "ingestion host", Default: "https://us.i.posthog.com", Options: []string{"https://us.i.posthog.com", "https://eu.i.posthog.com"}},
		{Name: "distinct_id", Default: "open-conduit@<hostname>"},
		{Name: "token", Help: "the project API key (phc_…)", Required: true, Secret: true},
	}
}

// entryFields describe a posthog entry's config.
func entryFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "turns", Help: "agent_turn events of the entry's scope", Default: "true"},
		{Name: "telemetry_events", Help: "forward introspection telemetry events", Default: "true"},
		{Name: "records", Help: `lifecycle records of the entry's scope to send, each as an event named after it: names, families such as "execution.*", or "*"`},
	}
}
