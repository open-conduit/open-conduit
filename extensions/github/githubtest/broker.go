package githubtest

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Broker is a fake App token broker: the service an operator runs that
// holds a GitHub App's private key so their daemons don't. It answers the
// four calls that authenticate as the App, in GitHub's shape (GET
// /repos/<repo>/installation, POST /app/installations/<id>/access_tokens,
// GET /app), to the daemons it has registered: each signs its JWT with a
// key of its own, and is limited to the repositories and installations it
// was registered with. It makes each call it allows to the GitHub fake with
// the App's real key, and refuses the rest with 404, as GitHub refuses an
// App a repository it isn't installed on.
type Broker struct {
	*httptest.Server
	upstream *Server
	appID    string
	appKey   *rsa.PrivateKey

	mu       sync.Mutex
	clients  map[string]BrokerClient
	requests []string
	refused  []string
}

// BrokerClient is one daemon a broker serves: the public half of the key it
// signs with, and the repositories (owner/name) and installation IDs it may
// use.
type BrokerClient struct {
	Key           *rsa.PublicKey
	Repos         []string
	Installations []int64
}

// NewBroker starts a broker for the App appID, whose key is appKey, in
// front of the GitHub fake upstream. The caller registers the App with the
// fake (upstream.AddApp) and the daemons with the broker (Register).
func NewBroker(upstream *Server, appID string, appKey *rsa.PrivateKey) *Broker {
	b := &Broker{upstream: upstream, appID: appID, appKey: appKey, clients: map[string]BrokerClient{}}
	b.Server = httptest.NewServer(http.HandlerFunc(b.serve))
	return b
}

// Register lets the daemon known as id (its JWT's iss) use the broker.
func (b *Broker) Register(id string, c BrokerClient) {
	b.mu.Lock()
	b.clients[id] = c
	b.mu.Unlock()
}

// Allow changes what the registered daemon id may use.
func (b *Broker) Allow(id string, repos []string, installations []int64) {
	b.mu.Lock()
	c := b.clients[id]
	c.Repos, c.Installations = repos, installations
	b.clients[id] = c
	b.mu.Unlock()
}

// Requests are the calls the broker was asked to make ("GET /app").
func (b *Broker) Requests() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.requests)
}

// Refused are the calls the broker refused, with why ("POST
// /app/installations/78/access_tokens: installation").
func (b *Broker) Refused() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.refused)
}

func (b *Broker) serve(w http.ResponseWriter, r *http.Request) {
	call := r.Method + " " + r.URL.Path
	b.mu.Lock()
	b.requests = append(b.requests, call)
	b.mu.Unlock()
	refuse := func(status int, why string) {
		b.mu.Lock()
		b.refused = append(b.refused, call+": "+why)
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"message":%q}`, http.StatusText(status))
	}
	client, ok := b.verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if !ok {
		refuse(http.StatusUnauthorized, "credentials")
		return
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/app":
	case r.Method == http.MethodGet && reInstall.MatchString(r.URL.Path):
		repo := reInstall.FindStringSubmatch(r.URL.Path)[1]
		if !slices.ContainsFunc(client.Repos, func(s string) bool { return strings.EqualFold(s, repo) }) {
			refuse(http.StatusNotFound, "repository")
			return
		}
	case r.Method == http.MethodPost && reAccessToken.MatchString(r.URL.Path):
		id, _ := strconv.ParseInt(reAccessToken.FindStringSubmatch(r.URL.Path)[1], 10, 64)
		if !slices.Contains(client.Installations, id) {
			refuse(http.StatusNotFound, "installation")
			return
		}
		// A scoped token may name only the daemon's repositories.
		var scope TokenScope
		if len(body) > 0 && json.Unmarshal(body, &scope) == nil {
			owner, _, _ := strings.Cut(b.upstream.Repo, "/")
			for _, name := range scope.Repositories {
				if !slices.ContainsFunc(client.Repos, func(s string) bool { return strings.EqualFold(s, owner+"/"+name) }) {
					refuse(http.StatusNotFound, "repository")
					return
				}
			}
		}
	default:
		refuse(http.StatusNotFound, "endpoint")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, b.upstream.URL+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		refuse(http.StatusBadGateway, "request")
		return
	}
	req.Header.Set("Authorization", "Bearer "+b.appJWT())
	req.Header.Set("Accept", r.Header.Get("Accept"))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		refuse(http.StatusBadGateway, "upstream")
		return
	}
	defer res.Body.Close()
	w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
	w.WriteHeader(res.StatusCode)
	_, _ = io.Copy(w, res.Body)
}

// verify checks a daemon's JWT: signed by the key registered for its iss
// and not expired by the GitHub fake's clock.
func (b *Broker) verify(tok string) (BrokerClient, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return BrokerClient{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return BrokerClient{}, false
	}
	var claims struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp < b.upstream.Now().Unix() {
		return BrokerClient{}, false
	}
	b.mu.Lock()
	client, ok := b.clients[claims.Iss]
	b.mu.Unlock()
	if !ok {
		return BrokerClient{}, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return BrokerClient{}, false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(client.Key, crypto.SHA256, sum[:], sig) != nil {
		return BrokerClient{}, false
	}
	return client, true
}

// appJWT is the App's own JWT, signed with its real key.
func (b *Broker) appJWT() string {
	enc := base64.RawURLEncoding
	now := b.upstream.Now()
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Unix() - 60, "exp": now.Unix() + 540, "iss": b.appID})
	unsigned := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, b.appKey, crypto.SHA256, sum[:])
	return unsigned + "." + enc.EncodeToString(sig)
}
