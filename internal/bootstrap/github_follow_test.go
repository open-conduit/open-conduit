package bootstrap

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/internal/config"
)

// TestFollowGitHubConnection: a registered entry's client follows its
// connection's applied config (here git "bot") without a restart; an
// unchanged connection leaves it alone, and a move to another api_url
// reports restart_required for the entry.
func TestFollowGitHubConnection(t *testing.T) {
	conn := func(cfg string) *config.Snapshot {
		return &config.Snapshot{Connections: []config.Connection{{ID: "gh", Type: gh.Type, Config: json.RawMessage(cfg), SecretRefs: map[string]string{"token": "env:T"}}}}
	}
	none := func(context.Context, string) (string, error) { return "", gh.ErrCredential }
	client, err := gh.NewClient(gh.ConnectionConfig{APIURL: "https://api.github.com"}, none, gh.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first := conn(`{"api_url":"https://api.github.com"}`)
	src := &githubSource{name: "github/work/gh", connection: "gh", conn: connectionKey(&first.Connections[0]), client: client}
	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), reported: map[string]string{}}
	const ns = "github/github.com/o/r"

	d.followGitHubConnection(first, ns, src)
	if client.GitBot() || d.reported["github\x00"+ns] != "" {
		t.Fatalf("unchanged connection: bot %v, reported %q", client.GitBot(), d.reported["github\x00"+ns])
	}
	bot := conn(`{"api_url":"https://api.github.com","bot_app_id":"7","git":"bot"}`)
	d.followGitHubConnection(bot, ns, src)
	if !client.GitBot() || src.conn != connectionKey(&bot.Connections[0]) {
		t.Fatal("the applied bot connection was not taken up")
	}
	d.followGitHubConnection(conn(`{"api_url":"https://ghe.example/api/v3"}`), ns, src)
	if got := d.reported["github\x00"+ns]; got != "restart_required" {
		t.Fatalf("another api_url: reported %q", got)
	}
	if !client.GitBot() {
		t.Fatal("a refused change altered the client")
	}
}
