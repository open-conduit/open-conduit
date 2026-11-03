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
		{Name: "channels", Required: true},
		{Name: "start"},
		{Name: "bot_user_id"},
		{Name: "start_window_hours"},
		{Name: "history_hours"},
		{Name: "refresh_minutes"},
		{Name: "receiver", Default: "socket", Options: []string{"socket", "events", "none"}},
		{Name: "signing_secret"},
		{Name: "projection"},
	}
}
