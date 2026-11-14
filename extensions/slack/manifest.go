package slack

import (
	"encoding/json"
	"net/url"
	"slices"
)

// The Slack app this extension needs, as one source of truth: the
// extension's setup (04-02, behind the generic extension Setup hook) opens
// Slack's create-app page with this manifest, and its doctor check compares
// a token's granted scopes with BotScopes.

// BotScopes are the bot token scopes the app is granted. The extension calls
// conversations.history and conversations.replies (channels:history,
// groups:history), chat.postMessage (chat:write), auth.test (none) and, in
// setup, users.conversations (channels:read, groups:read).
// metadata.message:read lets history and replies return the reply marker
// (message metadata) to this token, so marker recovery does not depend on the
// bot-user/text fallback (04-02 decision; see the extension doc). users:read,
// reactions:write and app_mentions:read are granted for names and
// acknowledgements, and are not required by this extension's calls.
var BotScopes = []string{"channels:history", "groups:history", "channels:read", "groups:read", "chat:write", "metadata.message:read", "reactions:write", "users:read", "app_mentions:read"}

// DirectScopes are the bot scopes direct messages need: im:history for
// conversations.history and conversations.replies in a DM, im:read to list
// the app's DMs (users.conversations) for reconcile. A reply in a DM is
// chat.postMessage (chat:write), so no im:write is asked for. The manifest
// grants them; doctor requires them only of an entry that turns direct
// messages on.
var DirectScopes = []string{"im:history", "im:read"}

// MarkerScope is the scope that lets reads return reply markers. A token
// without it still works through the text fallback; doctor reports it.
const MarkerScope = "metadata.message:read"

// BotEvents are the subscribed bot events. Push observes `message` events
// from public and private channels; app_mention duplicates a message event
// and is acknowledged and ignored.
var BotEvents = []string{"message.channels", "message.groups", "message.im", "app_mention"}

// AppTokenScopes are the app-level token's scopes (Socket Mode). Slack has no
// API to create this token: the user creates it on the app's settings page.
var AppTokenScopes = []string{"connections:write"}

// MethodScopes maps each Web API method the extension calls to the bot scopes
// that permit it (any one suffices; empty means none is needed).
var MethodScopes = map[string][]string{
	"conversations.history": {"channels:history", "groups:history", "im:history"},
	"conversations.replies": {"channels:history", "groups:history", "im:history"},
	"chat.postMessage":      {"chat:write"},
	"chat.postEphemeral":    {"chat:write"},
	"users.conversations":   {"channels:read", "groups:read", "im:read"},
	"auth.test":             {},
}

// AppMethods are called with the app-level token instead.
var AppMethods = map[string][]string{"apps.connections.open": {"connections:write"}}

// Manifest is the Slack app manifest: Socket Mode on, a bot user, BotScopes,
// DirectScopes and BotEvents, and the App Home messages tab, without which
// people cannot write to the app directly.
func Manifest(name string) map[string]any {
	if name == "" {
		name = "Open Conduit"
	}
	return map[string]any{
		"display_information": map[string]any{"name": name,
			"description": "Open Conduit support agent: answers threads in the channels it is invited to."},
		"features": map[string]any{"bot_user": map[string]any{"display_name": name, "always_online": true},
			"app_home": map[string]any{"home_tab_enabled": false, "messages_tab_enabled": true, "messages_tab_read_only_enabled": false}},
		"oauth_config": map[string]any{"scopes": map[string]any{"bot": append(slices.Clone(BotScopes), DirectScopes...)}},
		"settings": map[string]any{
			"event_subscriptions": map[string]any{"bot_events": BotEvents},
			// Interactivity carries a prompt's button presses, over Socket
			// Mode, so it needs no request URL.
			"interactivity":          map[string]any{"is_enabled": true},
			"org_deploy_enabled":     false,
			"socket_mode_enabled":    true,
			"token_rotation_enabled": false,
		},
	}
}

// ManifestJSON is the manifest as JSON.
func ManifestJSON(name string) []byte {
	data, _ := json.Marshal(Manifest(name))
	return data
}

// CreateAppURL opens Slack's create-app page prefilled with the manifest.
func CreateAppURL(name string) string {
	return "https://api.slack.com/apps?new_app=1&manifest_json=" + url.QueryEscape(string(ManifestJSON(name)))
}

// MissingScopes lists BotScopes a token lacks (granted is Slack's
// X-OAuth-Scopes list), for doctor.
func MissingScopes(granted []string) []string { return missing(granted, BotScopes) }

// MissingDirectScopes lists DirectScopes a token lacks.
func MissingDirectScopes(granted []string) []string { return missing(granted, DirectScopes) }

func missing(granted, want []string) []string {
	have := map[string]bool{}
	for _, s := range granted {
		have[s] = true
	}
	var out []string
	for _, s := range want {
		if !have[s] {
			out = append(out, s)
		}
	}
	return out
}
