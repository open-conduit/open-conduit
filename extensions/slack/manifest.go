package slack

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
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
// bot-user/text fallback (04-02 decision; see the extension doc). users:read
// lets users.info name the people a plan's request tasks are for (best
// effort: without it, they go unnamed). reactions:write and
// app_mentions:read are granted for acknowledgements.
// chat:write.customize (CustomizeScope) lets chat.postMessage post under a
// binding's own name and icon (the entry's identities). files:write
// (FilesScope) lets slack.upload reserve and share a file
// (files.getUploadURLExternal, files.completeUploadExternal).
var BotScopes = []string{"channels:history", "groups:history", "channels:read", "groups:read", "chat:write", "chat:write.customize", "metadata.message:read", "reactions:write", "users:read", "app_mentions:read", FilesScope}

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
	"conversations.history":        {"channels:history", "groups:history", "im:history"},
	"conversations.replies":        {"channels:history", "groups:history", "im:history"},
	"chat.postMessage":             {"chat:write"},
	"chat.postEphemeral":           {"chat:write"},
	"chat.update":                  {"chat:write"},
	"chat.getPermalink":            {},
	"reactions.add":                {"reactions:write"},
	"reactions.remove":             {"reactions:write"},
	"users.conversations":          {"channels:read", "groups:read", "im:read"},
	"files.getUploadURLExternal":   {FilesScope},
	"files.completeUploadExternal": {FilesScope},
	"auth.test":                    {},
}

// AppMethods are called with the app-level token instead.
var AppMethods = map[string][]string{"apps.connections.open": {"connections:write"}}

// Manifest is the Slack app manifest: Socket Mode on, a bot user, BotScopes,
// DirectScopes and BotEvents, and the App Home messages tab, without which
// people cannot write to the app directly.
func Manifest(name string) map[string]any { return AppManifest(name, nil) }

// AppURLs are an HTTP app's Request URLs on the daemon's webhook listener:
// the Events API's and interactivity's (button presses).
type AppURLs struct {
	Events        string
	Interactivity string
}

// HTTPURLs are the Request URLs of the connection conn under public, the
// URL that reaches the webhook listener: POST /slack/<conn> and POST
// /slack/<conn>/interactivity, which deliver to every receiver http entry
// of the connection.
func HTTPURLs(public, conn string) AppURLs {
	base := strings.TrimSuffix(public, "/") + "/" + Type + "/" + conn
	return AppURLs{Events: base, Interactivity: base + "/" + InteractivityPath}
}

// AppManifest is Manifest for an app that receives over Socket Mode (urls
// nil) or over HTTP at urls, with Socket Mode off.
func AppManifest(name string, urls *AppURLs) map[string]any {
	if name == "" {
		name = "Open Conduit"
	}
	events := map[string]any{"bot_events": BotEvents}
	// Interactivity carries button presses: over Socket Mode, which needs
	// no request URL, or to the interactivity Request URL.
	interactivity := map[string]any{"is_enabled": true}
	if urls != nil {
		events["request_url"] = urls.Events
		interactivity["request_url"] = urls.Interactivity
	}
	return map[string]any{
		"display_information": map[string]any{"name": name,
			"description": "Open Conduit support agent: answers threads in the channels it is invited to."},
		"features": map[string]any{"bot_user": map[string]any{"display_name": name, "always_online": true},
			"app_home": map[string]any{"home_tab_enabled": false, "messages_tab_enabled": true, "messages_tab_read_only_enabled": false}},
		"oauth_config": map[string]any{"scopes": map[string]any{"bot": append(slices.Clone(BotScopes), DirectScopes...)}},
		"settings": map[string]any{
			"event_subscriptions":    events,
			"interactivity":          interactivity,
			"org_deploy_enabled":     false,
			"socket_mode_enabled":    urls == nil,
			"token_rotation_enabled": false,
		},
	}
}

// ManifestJSON is the manifest as JSON.
func ManifestJSON(name string) []byte { return AppManifestJSON(name, nil) }

// AppManifestJSON is AppManifest as JSON.
func AppManifestJSON(name string, urls *AppURLs) []byte {
	data, _ := json.Marshal(AppManifest(name, urls))
	return data
}

// CreateAppURL opens Slack's create-app page prefilled with the manifest.
func CreateAppURL(name string) string { return CreateAppURLFor(name, nil) }

// CreateAppURLFor is CreateAppURL with AppManifest(name, urls).
func CreateAppURLFor(name string, urls *AppURLs) string {
	return "https://api.slack.com/apps?new_app=1&manifest_json=" + url.QueryEscape(string(AppManifestJSON(name, urls)))
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
