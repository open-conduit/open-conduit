package management

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// Verifier is a credential verifier type the host can trust an issuer of:
// the verifier itself, and its check of an issuer's config (nil accepts
// any object).
type Verifier struct {
	Verifier extension.CredentialVerifier
	Validate func(config json.RawMessage) error
}

// issuerRecord is one trusted issuer, which only an admin sets: the
// verifier type that checks its credentials, that verifier's config, and
// the table mapping the issuer's groups to roles. Keeping the table here,
// not in config, means a config apply can't grant a role (#366).
type issuerRecord struct {
	ID        string
	Type      string
	Config    json.RawMessage
	Groups    map[string]string `json:",omitempty"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// role is the highest role any of groups maps to; "" when none does, which
// may nothing.
func (r issuerRecord) role(groups []string) string {
	best := ""
	for _, g := range groups {
		if role := r.Groups[g]; roleRank[role] > roleRank[best] {
			best = role
		}
	}
	return best
}

// verifyTimeout bounds one credential.verify call. A verifier that answers
// slower than this fails the request closed.
const verifyTimeout = 5 * time.Second

// maxVerified bounds the cache of verified credentials.
const maxVerified = 4096

// maxSubject is the longest subject the host records in an actor.
const maxSubject = 256

// verified is a credential a verifier accepted, kept until it expires so
// the verifier isn't asked again on every request. It is tied to the
// issuer record it was verified under: changing or removing the issuer
// drops it.
type verified struct {
	issuer    string
	updatedAt time.Time
	subject   string
	groups    []string
	expiresAt time.Time
}

type verifiedCache struct {
	mu sync.Mutex
	m  map[string]verified
}

func (c *verifiedCache) get(digest string) (verified, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[digest]
	return v, ok
}

func (c *verifiedCache) put(digest string, v verified, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]verified{}
	}
	if len(c.m) >= maxVerified {
		for k, x := range c.m {
			if !now.Before(x.expiresAt) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= maxVerified {
			c.m = map[string]verified{}
		}
	}
	c.m[digest] = v
}

func (c *verifiedCache) drop(digest string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, digest)
}

// validSubject is a subject the host can record: 1 to maxSubject printable
// ASCII characters, no spaces.
func validSubject(s string) bool {
	if s == "" || len(s) > maxSubject {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// verifyIssued authenticates a bearer the host didn't issue by offering it
// to each trusted issuer in ID order, through the issuer's verifier. The
// first that accepts it names the operator: operator:<issuer>:<subject>,
// with the role the issuer's group table gives its groups. No issuer, an
// issuer whose verifier isn't running, a refusal, a verifier error, a
// missing or past expiry and an unusable subject are all 401, so the host
// fails closed.
func (s *Server) verifyIssued(ctx context.Context, token string) (principal, error) {
	unverified := fail(401, "unauthenticated", "missing or invalid bearer token")
	if s.o.Verifiers == nil {
		return principal{}, unverified
	}
	var issuers []issuerRecord
	if err := s.read(ctx, func(tx storage.Tx) error {
		var err error
		issuers, err = listIssuers(tx)
		return err
	}); err != nil {
		return principal{}, err
	}
	if len(issuers) == 0 {
		return principal{}, unverified
	}
	now := s.o.Now()
	digest := runtime.TokenDigest(token)
	if v, ok := s.verified.get(digest); ok {
		for _, r := range issuers {
			if r.ID == v.issuer && r.UpdatedAt.Equal(v.updatedAt) && now.Before(v.expiresAt) {
				return issuedPrincipal(r, v, token), nil
			}
		}
		s.verified.drop(digest)
	}
	for _, r := range issuers {
		ver, ok := s.o.Verifiers(r.Type)
		if !ok || ver.Verifier == nil {
			continue
		}
		vctx, cancel := context.WithTimeout(ctx, verifyTimeout)
		got, err := ver.Verifier.Verify(vctx, extension.CredentialCheck{Issuer: r.ID, Config: domain.Payload{Schema: extension.IssuerConfigSchema, Data: r.Config}, Token: token})
		cancel()
		if err != nil || !validSubject(got.Subject) || got.ExpiresAt.IsZero() || !now.Before(got.ExpiresAt) {
			continue
		}
		v := verified{issuer: r.ID, updatedAt: r.UpdatedAt, subject: got.Subject, groups: append([]string(nil), got.Groups...), expiresAt: got.ExpiresAt}
		s.verified.put(digest, v, now)
		return issuedPrincipal(r, v, token), nil
	}
	return principal{}, unverified
}

func issuedPrincipal(r issuerRecord, v verified, token string) principal {
	return principal{operator: true, token: token, name: r.ID + ":" + v.subject, role: r.role(v.groups), issuer: r.ID}
}

func listIssuers(tx storage.Tx) ([]issuerRecord, error) {
	rows, err := tx.List(authNamespace, "issuer/")
	if err != nil {
		return nil, err
	}
	out := make([]issuerRecord, 0, len(rows))
	for _, row := range rows {
		var r issuerRecord
		if err = json.Unmarshal(row.Value, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

type wireIssuer struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Config    json.RawMessage   `json:"config"`
	Groups    map[string]string `json:"groups"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
}

func toWireIssuer(r issuerRecord) wireIssuer {
	groups := r.Groups
	if groups == nil {
		groups = map[string]string{}
	}
	return wireIssuer{ID: r.ID, Type: r.Type, Config: r.Config, Groups: groups, CreatedAt: ts(r.CreatedAt), UpdatedAt: ts(r.UpdatedAt)}
}

// listIssuersRoute is GET /auth/issuers: every trusted issuer, with its
// config and group table.
func (s *Server) listIssuersRoute(c *call) (int, any, error) {
	var out []wireIssuer
	err := s.read(c.ctx, func(tx storage.Tx) error {
		rs, err := listIssuers(tx)
		for _, r := range rs {
			out = append(out, toWireIssuer(r))
		}
		return err
	})
	if err != nil {
		return 0, nil, err
	}
	if out == nil {
		out = []wireIssuer{}
	}
	return 200, map[string]any{"issuers": out}, nil
}

// maxGroup is the longest group name an issuer's table takes.
const maxGroup = 256

// putIssuer is PUT /auth/issuers/{id} (admin): trust an issuer, or replace
// its type, config and group table. The verifier type must be one a
// running package provides, and the package checks the config (validate,
// kind verifier) before it is stored. Each group maps to a role; a group
// left out grants nothing. Credentials verified under the previous record
// are verified again.
func (s *Server) putIssuer(c *call) (int, any, error) {
	id := c.params[0]
	var in struct {
		Type   string            `json:"type"`
		Config json.RawMessage   `json:"config"`
		Groups map[string]string `json:"groups"`
	}
	var out wireIssuer
	err := func() error {
		if err := c.decode(&in); err != nil {
			return err
		}
		if !ValidOperatorName(id) {
			return fail(400, "malformed", "an issuer ID must be 1-32 lower case letters, digits or dashes, start with a letter, and not be local")
		}
		if len(in.Config) == 0 || strings.TrimSpace(string(in.Config)) == "null" {
			in.Config = json.RawMessage(`{}`)
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(in.Config, &obj) != nil {
			return fail(400, "malformed", "config must be a JSON object")
		}
		for g, role := range in.Groups {
			if g == "" || len(g) > maxGroup || !ValidRole(role) {
				return fail(400, "malformed", "groups map each group name (1-256 characters) to admin, operate or view")
			}
		}
		var ver Verifier
		ok := false
		if s.o.Verifiers != nil {
			ver, ok = s.o.Verifiers(in.Type)
		}
		if !ok {
			return fail(422, "invalid_request", "type must be a credential verifier type a running package provides")
		}
		if ver.Validate != nil {
			if err := ver.Validate(in.Config); err != nil {
				return fail(422, "invalid_config", "the "+in.Type+" verifier refused this issuer config")
			}
		}
		now := s.o.Now()
		return s.o.Store.Transact(c.ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
			r := issuerRecord{ID: id, Type: in.Type, Config: in.Config, Groups: in.Groups, CreatedAt: now, UpdatedAt: now}
			if b, err := tx.Get(authNamespace, "issuer/"+id); err == nil {
				var prior issuerRecord
				if err = json.Unmarshal(b, &prior); err != nil {
					return err
				}
				r.CreatedAt = prior.CreatedAt
				// UpdatedAt ties cached verifications to the record; it
				// must move even when the clock hasn't.
				if !now.After(prior.UpdatedAt) {
					r.UpdatedAt = prior.UpdatedAt.Add(time.Nanosecond)
				}
			} else if !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			b, _ := json.Marshal(r)
			out = toWireIssuer(r)
			return tx.Put(authNamespace, "issuer/"+id, b)
		})
	}()
	s.observeOperator(c, "operator.issuer_set", id, "", err)
	if err != nil {
		return 0, nil, err
	}
	return 200, out, nil
}

// deleteIssuer is DELETE /auth/issuers/{id} (admin): stop trusting an
// issuer. Its credentials are refused from the next request on.
func (s *Server) deleteIssuer(c *call) (int, any, error) {
	id := c.params[0]
	err := s.o.Store.Transact(c.ctx, storage.Guard{Now: s.o.Now()}, func(tx storage.Tx) error {
		if _, err := tx.Get(authNamespace, "issuer/"+id); err != nil {
			return err
		}
		return tx.Delete(authNamespace, "issuer/"+id)
	})
	s.observeOperator(c, "operator.issuer_removed", id, "", err)
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"id": id}, nil
}
