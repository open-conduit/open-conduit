package github

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/open-conduit/open-conduit/protocol"
)

// A registered entry's client follows its connection's applied config
// (here git "bot") without a restart; an unchanged connection leaves it
// alone, and a move to another api_url can't be applied in place.
func TestFollowConnection(t *testing.T) {
	conn := func(cfg string) *protocol.ConnectionEntry {
		return &protocol.ConnectionEntry{ID: "gh", Type: Type, Config: json.RawMessage(cfg), Secrets: []string{"token"}}
	}
	none := func(context.Context, string) (string, error) { return "", ErrCredential }
	client, err := NewClient(ConnectionConfig{APIURL: "https://api.github.com"}, none, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	x := New()
	first := conn(`{"api_url":"https://api.github.com"}`)
	e := &entry{name: "github/work/gh", connection: "gh", conn: connectionKey(first), client: client}

	if !x.follow(e, first) || client.GitBot() {
		t.Fatalf("unchanged connection: bot %v", client.GitBot())
	}
	bot := conn(`{"api_url":"https://api.github.com","bot_app_id":"7","git":"bot"}`)
	if !x.follow(e, bot) || !client.GitBot() || e.conn != connectionKey(bot) {
		t.Fatal("the applied bot connection was not taken up")
	}
	if x.follow(e, conn(`{"api_url":"https://ghe.example/api/v3"}`)) {
		t.Fatal("another api_url was applied in place")
	}
	if !client.GitBot() {
		t.Fatal("a refused change altered the client")
	}
}
