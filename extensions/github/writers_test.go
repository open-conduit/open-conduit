package github

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// TestWritersClient: a binding's connection the host supplies is written as
// when it reaches the entry's GitHub; one the host doesn't supply leaves the
// binding on the entry's connection, and one on another GitHub or with an
// invalid config can't be used.
func TestWritersClient(t *testing.T) {
	configs := map[string]string{
		"reviewer": `{"bot_app_id":"2"}`,
		"ghe":      `{"api_url":"https://ghe.example/api/v3"}`,
		"broken":   `{"auth":"nope"}`,
	}
	secrets := func(context.Context, string) (string, error) { return "t", nil }
	w := NewWriters(func(scope domain.ID, id string) (json.RawMessage, SecretFunc, bool) {
		c, ok := configs[id]
		return json.RawMessage(c), secrets, ok && scope == "work"
	}, time.Now)
	entry, err := NewClient(ConnectionConfig{APIURL: "https://api.github.com"}, secrets, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := w.Client("work", "reviewer", entry)
	if err != nil || first == nil {
		t.Fatalf("reviewer: %v", err)
	}
	if again, _ := w.Client("work", "reviewer", entry); again != first {
		t.Fatal("client not kept while its config is unchanged")
	}
	if _, err := w.Client("other", "reviewer", entry); !errors.Is(err, errNotWriter) {
		t.Fatalf("a scope where the host doesn't supply it: %v", err)
	}
	for _, id := range []string{"ghe", "broken"} {
		if _, err := w.Client("work", id, entry); !errors.Is(err, ErrWriter) {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if _, err := (*Writers)(nil).Client("work", "reviewer", entry); !errors.Is(err, errNotWriter) {
		t.Fatalf("no resolver: %v", err)
	}
}
