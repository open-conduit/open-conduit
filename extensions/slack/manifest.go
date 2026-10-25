package slack

import (
	"encoding/json"
	"net/url"
)

// The Slack app this extension needs, as one source of truth: the
// extension's setup (04-02, behind the generic extension Setup hook) opens
// Slack's create-app page with this manifest, and its doctor check compares
// a token's granted scopes with BotScopes.

// BotScopes are the bot token scopes the app is granted. The extension calls
// conversations.history and conversations.replies (channels:history,
// groups:history), chat.postMessage (chat:write) and auth.test (none);
// channels:read, groups:read, users:read, reactions:write and
// app_mentions:read are granted for the setup channel picker, names and
// acknowledgements, and are not required by this extension's calls.
var BotScopes = []string{"channels:history", "groups:history", "channels:read", "groups:read", "chat:write", "reactions:write", "users:read", "app_mentions:read"}

// BotEvents are the subscribed bot events. Push observes `message` events
// from public and private channels; app_mention duplicates a message event
// and is acknowledged and ignored.
var BotEvents = []string{"message.channels", "message.groups", "app_mention"}

// AppTokenScopes are the app-level token's scopes (Socket Mode). Slack has no
// API to create this token: the user creates it on the app's settings page.
var AppTokenScopes = []string{"connections:write"}

// MethodScopes maps each Web API method the extension calls to the bot scopes
// that permit it (any one suffices; empty means none is needed).
var MethodScopes = map[string][]string{
	"conversations.history": {"channels:history", "groups:history"},
	"conversations.replies": {"channels:history", "groups:history"},
	"chat.postMessage":      {"chat:write"},
	"auth.test":             {},
}

// AppMethods are called with the app-level token instead.
var AppMethods = map[string][]string{"apps.connections.open": {"connections:write"}}

// Manifest is the Slack app manifest: Socket Mode on, a bot user, BotScopes
// and BotEvents.
func Manifest(name string) map[string]any {
	if name == "" {
		name = "Open Conduit"
	}
	return map[string]any{
		"display_information": map[string]any{"name": name,
			"description": "Open Conduit support agent: answers threads in the channels it is invited to."},
		"features":     map[string]any{"bot_user": map[string]any{"display_name": name, "always_online": true}},
		"oauth_config": map[string]any{"scopes": map[string]any{"bot": BotScopes}},
		"settings": map[string]any{
			"event_subscriptions":    map[string]any{"bot_events": BotEvents},
			"interactivity":          map[string]any{"is_enabled": false},
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
func MissingScopes(granted []string) []string {
	have := map[string]bool{}
	for _, s := range granted {
		have[s] = true
	}
	var out []string
	for _, s := range BotScopes {
		if !have[s] {
			out = append(out, s)
		}
	}
	return out
}
