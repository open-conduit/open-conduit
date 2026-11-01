package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClientReconfigure: a reconfigured client writes with the connection's
// new credentials and takes up a bot App and git "bot" it didn't have, and a
// connection on another api_url is refused, leaving the client as it was.
func TestClientReconfigure(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()
	token := func(v string) SecretFunc {
		return func(_ context.Context, name string) (string, error) {
			if name != SecretToken {
				return "", ErrCredential
			}
			return v, nil
		}
	}
	c, err := NewClient(ConnectionConfig{APIURL: srv.URL}, token("old"), ClientOptions{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Comment(ctx, "o/r", 1, "x"); err != nil || auth != "Bearer old" {
		t.Fatalf("before: %q %v", auth, err)
	}
	if err := c.Reconfigure(ConnectionConfig{APIURL: srv.URL + "/"}, token("new")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Comment(ctx, "o/r", 1, "x"); err != nil || auth != "Bearer new" {
		t.Fatalf("after a credential change: %q %v", auth, err)
	}
	if c.hasBot() || c.GitBot() {
		t.Fatal("bot before one is configured")
	}
	if err := c.Reconfigure(ConnectionConfig{APIURL: srv.URL, BotAppID: "7", Git: GitBot}, token("new")); err != nil {
		t.Fatal(err)
	}
	if !c.hasBot() || !c.GitBot() {
		t.Fatal("bot App and git bot not taken up")
	}
	if err := c.Reconfigure(ConnectionConfig{APIURL: "https://ghe.example/api/v3"}, token("ghe")); !errors.Is(err, ErrReconfigure) {
		t.Fatalf("another api_url: %v", err)
	}
	if !c.hasBot() {
		t.Fatal("a refused reconfigure changed the client")
	}
	if err := c.Reconfigure(ConnectionConfig{APIURL: srv.URL, Auth: "nope"}, token("x")); err == nil || !c.hasBot() {
		t.Fatalf("unknown auth: %v", err)
	}
}
