package slack

import "github.com/open-conduit/open-conduit/protocol"

// connectionFields describe the slack connection's config and secret_refs
// for the operator's catalogue (manifest fields).
func connectionFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "api_url", Default: "https://slack.com/api"},
		{Name: SecretBotToken, Help: "xoxb-…", Required: true, Secret: true},
		{Name: SecretAppToken, Help: "xapp-… for Socket Mode", Secret: true},
	}
}

// entryFields describe a slack entry's config.
func entryFields() []protocol.FieldDecl {
	return []protocol.FieldDecl{
		{Name: "namespace", Help: "stable source identity and correlation-key source", Required: true},
		{Name: "scope", Help: "the containing scope's id", Required: true},
		{Name: "team", Required: true},
		{Name: "channels", Help: "1-100 channel IDs to observe; none needs direct_messages"},
		{Name: "direct_messages", Help: `{"users": ["U…"]} or {"anyone": true}: a direct message with the app starts Work, from those people only`},
		{Name: "start"},
		{Name: "bot_user_id"},
		{Name: "start_window_hours"},
		{Name: "claim_from", Help: "registration (threads begun since this daemon first registered the channel), all, or an RFC 3339 time", Default: "registration"},
		{Name: "history_hours"},
		{Name: "refresh_minutes"},
		{Name: "receiver", Default: "socket", Options: []string{"socket", "events", "none"}},
		{Name: "signing_secret"},
		{Name: "projection", Help: `{"disabled", "dry_run", "format": "markdown" (default, text read as standard markdown) or "mrkdwn" (Slack's own syntax)}`},
		{Name: "post"},
		{Name: "progress", Help: `{"enabled": true, "stop": ["slack:U…"]}: one live progress message per run in its thread, with a Stop button for those listed`},
		{Name: "identities", Help: "a binding's own name and icon: [{binding, username, icon_emoji or icon_url}]"},
		{Name: "request_messages", Help: `{"enabled": true}: a direct message to each Slack person a request names as a responder, with its buttons, changed to the result once it settles`},
		{Name: "uploads", Help: "turns slack.upload on: {max_bytes (default and at most 10 MB), media_types: [\"image/png\", \"image/*\" or \"*/*\"]}, checked before anything is sent"},
		{Name: "feedback", Help: `{"enabled": true, "bindings": ["…"]}: thumbs up and down below replies, of the bindings listed or of every binding`},
		{Name: "child_requests", Help: "true shows the requests of a conversation's child Work (an approval, a question) in its thread, with buttons when a Slack person may answer", Default: "false"},
	}
}
