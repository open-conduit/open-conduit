package management

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/observability"
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
	ID     string
	Type   string
	Config json.RawMessage
	Groups map[string]string `json:",omitempty"`
	// Grants map groups to roles at scopes: a member of a row's group
	// holds its role at its scope's subtree, beside the daemon-wide role
	// Groups gives.
	Grants    []groupGrant `json:",omitempty"`
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
			s.observeCredential(ctx, r, digest, "", "no_verifier", "no running package provides the "+r.Type+" verifier", token)
			continue
		}
		vctx, cancel := context.WithTimeout(ctx, verifyTimeout)
		got, err := ver.Verifier.Verify(vctx, extension.CredentialCheck{Issuer: r.ID, Config: domain.Payload{Schema: extension.IssuerConfigSchema, Data: r.Config}, Token: token})
		timedOut := vctx.Err() != nil && ctx.Err() == nil
		cancel()
		var code, detail string
		switch {
		case timedOut:
			code, detail = "timeout", "the verifier didn't answer within "+verifyTimeout.String()
		case errors.Is(err, extension.ErrUnauthorized):
			code, detail = "refused", err.Error()
		case err != nil:
			code, detail = "verifier_failed", err.Error()
		case !validSubject(got.Subject):
			code, detail = "invalid_subject", "the verifier named no subject of 1 to 256 printable characters without spaces"
		case got.ExpiresAt.IsZero():
			code, detail = "no_expiry", "the verifier gave no expiry"
		case !now.Before(got.ExpiresAt):
			code, detail = "expired", "the credential expired at "+ts(got.ExpiresAt)
		}
		if code != "" {
			s.observeCredential(ctx, r, digest, "", code, detail, token)
			continue
		}
		v := verified{issuer: r.ID, updatedAt: r.UpdatedAt, subject: got.Subject, groups: append([]string(nil), got.Groups...), expiresAt: got.ExpiresAt}
		s.verified.put(digest, v, now)
		p := issuedPrincipal(r, v, token)
		s.observeCredential(ctx, r, digest, p.actor(), "", "", token)
		return p, nil
	}
	return principal{}, unverified
}

// maxCredentialDetail bounds the reason a credential record carries.
const maxCredentialDetail = 300

// observeCredential records one verification by a trusted issuer: who it
// named (auth.credential_verified, with operator.actor and its expiry) or
// why it refused (auth.credential_refused, with a code and the verifier's
// reason, the token itself redacted). It is recorded each time a verifier
// is asked, not for a credential the cache already holds. The response
// stays a bare 401.
func (s *Server) observeCredential(ctx context.Context, r issuerRecord, credential, actor, code, detail, token string) {
	if s.o.Observer == nil {
		return
	}
	at := s.o.Now()
	trace := digest("auth", r.ID, credential, at.UTC().Format(time.RFC3339Nano))
	attrs := map[string]string{"auth.issuer": r.ID, "auth.type": r.Type, "auth.credential": credential[:16], "record_id": trace}
	name := "auth.credential_verified"
	if code == "" {
		attrs["operator.actor"] = actor
	} else {
		name = "auth.credential_refused"
		attrs["auth.code"] = code
		detail = strings.ReplaceAll(detail, token, "[REDACTED]")
		if len(detail) > maxCredentialDetail {
			detail = detail[:maxCredentialDetail]
		}
		attrs["auth.reason"] = detail
	}
	rec := observability.Record{Name: name, At: at, Correlation: observability.Correlation{TraceID: trace}, Attributes: attrs}
	if observability.Validate(rec) != nil {
		return
	}
	_ = s.o.Observer.Observe(context.WithoutCancel(ctx), observability.Redact(rec, []string{token}))
}

func issuedPrincipal(r issuerRecord, v verified, token string) principal {
	return principal{operator: true, token: token, name: r.ID + ":" + v.subject, role: r.role(v.groups), grants: grantsOf(r.Grants, v.groups), issuer: r.ID}
}

// IssuerTypes are the verifier types the stored trusted issuers name, in
// order and each once, so the host can start the packages that verify
// their credentials.
func IssuerTypes(ctx context.Context, store storage.Store, now func() time.Time) ([]string, error) {
	var out []string
	err := store.Transact(ctx, storage.Guard{Now: now()}, func(tx storage.Tx) error {
		issuers, err := listIssuers(tx)
		for _, r := range issuers {
			if !slices.Contains(out, r.Type) {
				out = append(out, r.Type)
			}
		}
		return err
	})
	sort.Strings(out)
	return out, err
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
	Grants    []groupGrant      `json:"grants"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
}

func toWireIssuer(r issuerRecord) wireIssuer {
	groups := r.Groups
	if groups == nil {
		groups = map[string]string{}
	}
	grants := r.Grants
	if grants == nil {
		grants = []groupGrant{}
	}
	return wireIssuer{ID: r.ID, Type: r.Type, Config: r.Config, Groups: groups, Grants: grants, CreatedAt: ts(r.CreatedAt), UpdatedAt: ts(r.UpdatedAt)}
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
// left out grants nothing. Grants, rows of {group, scope, role}, give a
// group's members view or operate at a scope's subtree instead.
// Credentials verified under the previous record are verified again.
func (s *Server) putIssuer(c *call) (int, any, error) {
	id := c.params[0]
	var in struct {
		Type   string            `json:"type"`
		Config json.RawMessage   `json:"config"`
		Groups map[string]string `json:"groups"`
		Grants []groupGrant      `json:"grants"`
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
		if len(in.Grants) > maxGrants*4 {
			return fail(400, "malformed", "grants take up to 256 rows")
		}
		seen := map[groupGrant]bool{}
		for _, gg := range in.Grants {
			key := groupGrant{Group: gg.Group, Scope: gg.Scope}
			if gg.Group == "" || len(gg.Group) > maxGroup || !validGrants([]Grant{{Scope: gg.Scope, Role: gg.Role}}) || seen[key] {
				return fail(400, "malformed", "grants are rows of {group, scope, role}: a group name (1-256 characters), a scope ID and view or operate, no group and scope twice; admin is only ever daemon-wide")
			}
			seen[key] = true
		}
		if s.o.StartVerifier != nil && s.o.Verifiers != nil {
			if _, running := s.o.Verifiers(in.Type); !running {
				s.o.StartVerifier(c.ctx, in.Type)
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
			r := issuerRecord{ID: id, Type: in.Type, Config: in.Config, Groups: in.Groups, Grants: in.Grants, CreatedAt: now, UpdatedAt: now}
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
