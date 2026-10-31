package github

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// ErrWriter means a binding's connection can't be written as: the host no
// longer supplies it, its config doesn't parse, or it reaches another GitHub
// (api_url) than the entry's repository.
var ErrWriter = errors.New("github writer connection unavailable")

// errNotWriter means the connection isn't one this extension may act as in
// the scope (not a `github` connection, say a binding's `slack`): the binding
// writes as the entry's connection.
var errNotWriter = errors.New("not a github connection in scope")

// ConnectionFunc is the host's resolver for a connection the extension may
// act as in a scope (an entry's, or one a binding there names): its config
// and lazy secrets, or ok false when the extension may not act as it.
type ConnectionFunc func(scope domain.ID, id string) (config json.RawMessage, secrets SecretFunc, ok bool)

// Writers keeps one client per connection a binding names, so a binding's
// comments and reviews go out as its own `github` connection (a reviewer's
// App) while the entry's connection keeps reading and ingesting. A client is
// remade when the connection's config changes.
type Writers struct {
	conn ConnectionFunc
	now  func() time.Time

	mu      sync.Mutex
	clients map[string]writerClient
}

type writerClient struct {
	config string
	client *Client
}

// NewWriters returns the writer clients the host's resolver supplies; a nil
// resolver leaves every binding on its entry's connection.
func NewWriters(conn ConnectionFunc, now func() time.Time) *Writers {
	return &Writers{conn: conn, now: now, clients: map[string]writerClient{}}
}

// Client is the client of connection id in scope, which must reach the same
// GitHub as the entry's client, and goes out over the entry's HTTP client.
// It returns errNotWriter when the extension may not act as id, and
// ErrWriter when id can't be used.
func (w *Writers) Client(scope domain.ID, id string, entry *Client) (*Client, error) {
	if w == nil || w.conn == nil {
		return nil, errNotWriter
	}
	raw, secrets, ok := w.conn(scope, id)
	if !ok {
		return nil, errNotWriter
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	key := string(scope) + "\x00" + id
	if c, ok := w.clients[key]; ok && c.config == string(raw) {
		return c.client, nil
	}
	cfg, err := ParseConnection(raw)
	if err != nil {
		return nil, ErrWriter
	}
	if entry != nil && !strings.EqualFold(cfg.APIURL, entry.api) {
		return nil, ErrWriter
	}
	opts := ClientOptions{Now: w.now}
	if entry != nil {
		opts.HTTP = entry.base
	}
	client, err := NewClient(cfg, secrets, opts)
	if err != nil {
		return nil, ErrWriter
	}
	w.clients[key] = writerClient{config: string(raw), client: client}
	return client, nil
}
